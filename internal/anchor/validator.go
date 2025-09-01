package anchor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	k8sadm "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/config"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/webhooks"
)

const (
	// ServingPath is where the validator will run. Must be kept in sync
	// with the kubebuilder markers below.
	ServingPath = "/validate-hnc-x-k8s-io-v1alpha2-subnamespaceanchors"
)

// Note: the validating webhook FAILS CLOSE. This means that if the webhook goes down, all further
// changes are forbidden.
//
// +kubebuilder:webhook:admissionReviewVersions=v1,path=/validate-hnc-x-k8s-io-v1alpha2-subnamespaceanchors,mutating=false,failurePolicy=fail,groups="hnc.x-k8s.io",resources=subnamespaceanchors,sideEffects=None,verbs=create;update;delete,versions=v1alpha2,name=subnamespaceanchors.hnc.x-k8s.io

type Validator struct {
	Log    logr.Logger
	Forest *forest.Forest
}

func NewValidator(forest *forest.Forest) *Validator {
	return &Validator{
		Log:    ctrl.Log.WithName("anchor").WithName("validate"),
		Forest: forest,
	}
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateAnchor(ctx, obj, k8sadm.Create)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.validateAnchor(ctx, newObj, k8sadm.Update)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateAnchor(ctx, obj, k8sadm.Delete)
}

func (v *Validator) validateAnchor(ctx context.Context, obj runtime.Object, operation k8sadm.Operation) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	log := v.Log.WithValues("ns", req.Namespace, "nm", req.Name, "op", operation, "user", req.UserInfo.Username)

	if webhooks.IsHNCServiceAccount(&req.UserInfo) {
		log.V(1).Info("Allowed change by HNC SA")
		return nil, nil
	}

	anchor, ok := obj.(*api.SubnamespaceAnchor)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("expected a SubnamespaceAnchor but got a %T", obj))
	}

	anchorReq := &anchorRequest{
		anchor: anchor,
		op:     operation,
	}

	if err := v.handleValidation(anchorReq); err != nil {
		if !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) && !apierrors.IsForbidden(err) {
			log.Error(err, "Validation failed")
		} else {
			log.Info("Denied", "reason", err)
		}
		return nil, err
	}

	log.V(1).Info("Allowed")
	return nil, nil
}

// req defines the aspects of the admission.Request that we care about.
type anchorRequest struct {
	anchor *api.SubnamespaceAnchor
	op     k8sadm.Operation
}

// handleValidation implements the non-boilerplate logic of this validator, allowing it to be more easily unit
// tested (ie without constructing a full admission.Request). It validates that the request is allowed
// based on the current in-memory state of the forest.
func (v *Validator) handleValidation(req *anchorRequest) error {
	v.Forest.Lock()
	defer v.Forest.Unlock()
	pnm := req.anchor.Namespace
	cnm := req.anchor.Name
	cns := v.Forest.Get(cnm)

	if req.op != k8sadm.Delete {
		errStrs := validation.IsDNS1123Label(cnm)
		if len(errStrs) != 0 {
			fldPath := field.NewPath("metadata", "name")
			msg := fmt.Sprintf("not a valid namespace name: %s", strings.Join(errStrs, "; "))
			allErrs := field.ErrorList{
				field.Invalid(fldPath, cnm, msg),
			}
			return apierrors.NewInvalid(api.SubnamespaceAnchorGK, cnm, allErrs)
		}
	}

	labelErrs := config.ValidateManagedLabels(req.anchor.Spec.Labels)
	annotationErrs := config.ValidateManagedAnnotations(req.anchor.Spec.Annotations)
	allErrs := append(labelErrs, annotationErrs...)
	if len(allErrs) > 0 {
		return apierrors.NewInvalid(api.SubnamespaceAnchorGK, req.anchor.Name, allErrs)
	}

	switch req.op {
	case k8sadm.Create:
		// Can't create subnamespaces in unmanaged namespaces
		if why := config.WhyUnmanaged(pnm); why != "" {
			err := fmt.Errorf("cannot create a subnamespace in the unmanaged namespace %q (%s)", pnm, why)
			return apierrors.NewForbidden(api.SubnamespaceAnchorGR, pnm, err)
		}
		// Can't create subnamespaces using unmanaged namespace names
		if why := config.WhyUnmanaged(cnm); why != "" {
			err := fmt.Errorf("cannot create a subnamespace using the unmanaged namespace name %q (%s)", cnm, why)
			return apierrors.NewForbidden(api.SubnamespaceAnchorGR, cnm, err)
		}

		// Can't create anchors for existing namespaces, _unless_ it's for a subns with a missing
		// anchor.
		if cns.Exists() {
			childIsMissingAnchor := (cns.Parent().Name() == pnm && cns.IsSub)
			if !childIsMissingAnchor {
				err := errors.New("cannot create a subnamespace using an existing namespace")
				return apierrors.NewConflict(api.SubnamespaceAnchorGR, cnm, err)
			}
		}

	case k8sadm.Delete:
		// Don't allow the anchor to be deleted if it's in a good state and has descendants of its own,
		// unless allowCascadingDeletion is set.
		if req.anchor.Status.State == api.Ok && cns.ChildNames() != nil && !cns.AllowsCascadingDeletion() {
			err := fmt.Errorf("subnamespace %s is not a leaf and doesn't allow cascading deletion. Please set allowCascadingDeletion flag or make it a leaf first", cnm)
			return apierrors.NewForbidden(api.SubnamespaceAnchorGR, cnm, err)
		}

	default:
		// nop for updates etc
	}

	return nil
}
