package namespaces

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	k8sadm "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
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
	ServingPath = "/validate-v1-namespace"
)

var (
	namespaceGR = corev1.Resource("namespaces")
)

// Note: the validating webhook FAILS CLOSE. This means that if the webhook goes down, all further
// changes are forbidden.
//
// +kubebuilder:webhook:admissionReviewVersions=v1,path=/validate-v1-namespace,mutating=false,failurePolicy=fail,groups="",resources=namespaces,sideEffects=None,verbs=delete;create;update,versions=v1,name=namespaces.hnc.x-k8s.io

type Validator struct {
	Log    logr.Logger
	Forest *forest.Forest
}

func NewValidator(forest *forest.Forest) *Validator {
	return &Validator{
		Log:    ctrl.Log.WithName("namespace").WithName("validate"),
		Forest: forest,
	}
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateNamespace(ctx, obj, nil, k8sadm.Create)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.validateNamespace(ctx, newObj, oldObj, k8sadm.Update)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validateNamespace(ctx, obj, nil, k8sadm.Delete)
}

func (v *Validator) validateNamespace(ctx context.Context, obj runtime.Object, oldObj runtime.Object, operation k8sadm.Operation) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	log := v.Log.WithValues("nm", req.Name, "op", operation, "user", req.UserInfo.Username)

	if webhooks.IsHNCServiceAccount(&req.UserInfo) {
		log.V(1).Info("Allowed change by HNC SA")
		return nil, nil
	}

	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("expected a Namespace but got a %T", obj))
	}

	var oldns *corev1.Namespace
	if oldObj != nil {
		oldns, ok = oldObj.(*corev1.Namespace)
		if !ok {
			return nil, apierrors.NewInternalError(fmt.Errorf("expected a Namespace for oldObj but got a %T", oldObj))
		}
	}

	nsReq := &nsRequest{
		ns:    ns,
		oldns: oldns,
		op:    operation,
	}

	if err := v.handleValidation(nsReq); err != nil {
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

// nsRequest defines the aspects of the admission.Request that we care about.
type nsRequest struct {
	ns    *corev1.Namespace
	oldns *corev1.Namespace
	op    k8sadm.Operation
}

// handleValidation implements the non-boilerplate logic of this validator, allowing it to be more easily unit
// tested (ie without constructing a full admission.Request).
func (v *Validator) handleValidation(req *nsRequest) error {
	v.Forest.Lock()
	defer v.Forest.Unlock()

	ns := v.Forest.Get(req.ns.Name)

	switch req.op {
	case k8sadm.Create:
		if err := v.illegalIncludedNamespaceLabel(req); err != nil {
			return err
		}
		// This check only applies to the Create operation since namespace name
		// cannot be updated.
		if err := v.nameExistsInExternalHierarchy(req); err != nil {
			return err
		}

		if err := v.illegalTreeLabel(req); err != nil {
			return err
		}

	case k8sadm.Update:
		if err := v.illegalIncludedNamespaceLabel(req); err != nil {
			return err
		}
		// This check only applies to the Update operation. Creating a namespace
		// with external manager is allowed and we will prevent this conflict by not
		// allowing setting a parent when validating the HierarchyConfiguration.
		if err := v.conflictBetweenParentAndExternalManager(req, ns); err != nil {
			return err
		}

		if err := v.illegalTreeLabel(req); err != nil {
			return err
		}

	case k8sadm.Delete:
		if err := v.cannotDeleteSubnamespace(req); err != nil {
			return err
		}
		if err := v.illegalCascadingDeletion(ns); err != nil {
			return err
		}
	}

	return nil
}

// illegalTreeLabel checks if tree labels are being created or modified
// by any user or service account since only HNC service account is
// allowed to do so
func (v *Validator) illegalTreeLabel(req *nsRequest) error {
	oldLabels := map[string]string{}
	if req.oldns != nil {
		oldLabels = req.oldns.Labels
	}
	// Ensure the users hasn't added or changed any tree labels
	for key, val := range req.ns.Labels {
		if !strings.Contains(key, api.LabelTreeDepthSuffix) {
			continue
		}

		// Check if new HNC label tree key isn't being added
		if oldLabels[key] != val {
			err := fmt.Errorf("cannot set or modify tree label %q in namespace %q; these can only be managed by HNC", key, req.ns.Name)
			return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
		}
	}

	for key := range oldLabels {
		//  Make sure nothing's been deleted
		if strings.Contains(key, api.LabelTreeDepthSuffix) {
			if _, ok := req.ns.Labels[key]; !ok {
				err := fmt.Errorf("cannot remove tree label %q in namespace %q; these can only be managed by HNC", key, req.ns.Name)
				return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
			}
		}
	}

	return nil
}

// illegalIncludedNamespaceLabel checks if there's any illegal use of the
// included-namespace label on namespaces. It only checks a Create or an Update
// request.
func (v *Validator) illegalIncludedNamespaceLabel(req *nsRequest) error {
	// Early exit if there's no change on the label.
	labelValue, hasLabel := req.ns.Labels[api.LabelIncludedNamespace]
	if req.oldns != nil {
		oldLabelValue, oldHasLabel := req.oldns.Labels[api.LabelIncludedNamespace]
		if oldHasLabel == hasLabel && oldLabelValue == labelValue {
			return nil
		}
	}

	isIncluded := config.IsManagedNamespace(req.ns.Name)

	// An excluded namespaces should not have included-namespace label.
	if !isIncluded && hasLabel {
		err := fmt.Errorf("you cannot enforce webhook rules on this unmanaged namespace using the %q label. "+
			"See https://github.com/kubernetes-sigs/hierarchical-namespaces/blob/master/docs/user-guide/concepts.md#included-namespace-label "+
			"for detail", api.LabelIncludedNamespace)
		return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
	}

	// An included-namespace should have the included-namespace label with the
	// right value.
	// Note: since we have a mutating webhook to set the correct label if it's
	// missing before this, we only need to check if the label value is correct.
	if isIncluded && labelValue != "true" {
		err := fmt.Errorf("you cannot change the value of the %q label. It has to be set as true on all managed namespaces. "+
			"See https://github.com/kubernetes-sigs/hierarchical-namespaces/blob/master/docs/user-guide/concepts.md#included-namespace-label "+
			"for detail", api.LabelIncludedNamespace)
		return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
	}

	return nil
}

// nameExistsInExternalHierarchy only applies to the Create operation since
// namespace name cannot be updated.
func (v *Validator) nameExistsInExternalHierarchy(req *nsRequest) error {
	for _, nm := range v.Forest.GetNamespaceNames() {
		ns := v.Forest.Get(nm)
		if !ns.IsExternal() {
			continue
		}
		externalTreeLabels := ns.GetTreeLabels()
		if _, ok := externalTreeLabels[req.ns.Name]; ok {
			msg := fmt.Errorf("is reserved by the external hierarchy manager %q", v.Forest.Get(nm).Manager)
			return apierrors.NewConflict(namespaceGR, req.ns.Name, msg)
		}
	}
	return nil
}

// conflictBetweenParentAndExternalManager only applies to the Update operation.
// Creating a namespace with external manager is allowed and we will prevent
// this conflict by not allowing setting a parent when validating the
// HierarchyConfiguration.
func (v *Validator) conflictBetweenParentAndExternalManager(req *nsRequest, ns *forest.Namespace) error {
	mgr := req.ns.Annotations[api.AnnotationManagedBy]
	if mgr != "" && mgr != api.MetaGroup && ns.Parent() != nil {
		err := fmt.Errorf("is a child of %q. Namespaces with parents defined by HNC cannot also be managed externally. "+
			"To manage this namespace with %q, first make it a root in HNC", ns.Parent().Name(), mgr)
		return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
	}
	return nil
}

// cannotDeleteSubnamespace only applies to the Delete operation.
func (v *Validator) cannotDeleteSubnamespace(req *nsRequest) error {
	parent := req.ns.Annotations[api.SubnamespaceOf]
	// Early exit if the namespace is not a subnamespace.
	if parent == "" {
		return nil
	}

	// If the anchor doesn't exist, we want to allow it to be deleted anyway.
	// See issue https://github.com/kubernetes-sigs/hierarchical-namespaces/issues/847.
	anchorExists := v.Forest.Get(parent).HasAnchor(req.ns.Name)
	if anchorExists {
		err := fmt.Errorf("is a subnamespace. Please delete the anchor from the parent namespace %s to delete the subnamespace", parent)
		return apierrors.NewForbidden(namespaceGR, req.ns.Name, err)
	}
	return nil
}

func (v *Validator) illegalCascadingDeletion(ns *forest.Namespace) error {
	if ns.AllowsCascadingDeletion() {
		return nil
	}

	for _, cnm := range ns.ChildNames() {
		if v.Forest.Get(cnm).IsSub {
			err := errors.New("contains subnamespaces. Please remove all subnamespaces before deleting this namespace, or set 'allowCascadingDeletion' to delete them automatically")
			return apierrors.NewForbidden(namespaceGR, ns.Name(), err)
		}
	}
	return nil
}
