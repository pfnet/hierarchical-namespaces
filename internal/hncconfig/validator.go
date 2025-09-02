package hncconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	k8sadm "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/apimeta"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/selectors"
	"sigs.k8s.io/hierarchical-namespaces/internal/webhooks"
)

const (
	// ServingPath is where the validator will run. Must be kept in sync
	// with the kubebuilder markers below.
	ServingPath = "/validate-hnc-x-k8s-io-v1alpha2-hncconfigurations"
)

// Note: the validating webhook FAILS CLOSE. This means that if the webhook goes down, all further
// changes are denied.
//
// +kubebuilder:webhook:admissionReviewVersions=v1,path=/validate-hnc-x-k8s-io-v1alpha2-hncconfigurations,mutating=false,failurePolicy=fail,groups="hnc.x-k8s.io",resources=hncconfigurations,sideEffects=None,verbs=create;update;delete,versions=v1alpha2,name=hncconfigurations.hnc.x-k8s.io

type Validator struct {
	Log    logr.Logger
	Forest *forest.Forest
	mapper resourceMapper
}

type gvkSet map[schema.GroupVersionKind]api.SynchronizationMode

func NewValidator(forest *forest.Forest) *Validator {
	return &Validator{
		Log:    ctrl.Log.WithName("hncconfig").WithName("validate"),
		Forest: forest,
	}
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, obj, k8sadm.Create)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, newObj, k8sadm.Update)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, obj, k8sadm.Delete)
}

func (v *Validator) handle(ctx context.Context, obj runtime.Object, operation k8sadm.Operation) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	log := v.Log.WithValues("nm", req.Name, "op", operation, "user", req.UserInfo.Username)
	if webhooks.IsHNCServiceAccount(&req.UserInfo) {
		return nil, nil
	}

	if operation == k8sadm.Delete {
		if req.Name == api.HNCConfigSingleton {
			err := errors.New("deleting the 'config' object is forbidden")
			return nil, apierrors.NewForbidden(api.HNCConfigurationGR, api.HNCConfigSingleton, err)
		} else {
			return nil, nil
		}
	}

	inst, ok := obj.(*api.HNCConfiguration)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("expected a HNCConfiguration but got a %T", obj))
	}

	if err := v.validateInstance(inst); err != nil {
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

// validateInstance implements the validation logic of this validator for Create and Update operations,
// allowing it to be more easily unit tested (ie without constructing a full admission.Request).
func (v *Validator) validateInstance(inst *api.HNCConfiguration) error {
	ts := gvkSet{}
	// Convert all valid types from GR to GVK. If any type is invalid, e.g. not
	// exist in the apiserver, wrong configuration, deny the request.
	if err := v.validateTypes(inst, ts); err != nil {
		return err
	}

	// Lastly, check if changing a type to "Propagate" mode would cause
	// overwriting user-created objects.
	return v.checkForest(ts)
}

func (v *Validator) validateTypes(inst *api.HNCConfiguration, ts gvkSet) error {
	allErrs := field.ErrorList{}
	for i, r := range inst.Spec.Resources {
		gr := schema.GroupResource{Group: r.Group, Resource: r.Resource}
		fldPath := field.NewPath("spec", "resources").Index(i)
		// Validate the type configured is not an HNC enforced type.
		if api.IsEnforcedType(r) {
			fldErr := field.Invalid(fldPath, gr, "always uses the 'Propagate' mode and cannot be configured")
			allErrs = append(allErrs, fldErr)
		}

		// Validate the resource exists and is namespaced. And convert GR to GVK.
		// We use GVK because we will need to checkForest() later to avoid source
		// overwriting conflict (forest uses GVK as the key for object reconcilers).
		gvk, err := v.mapper.NamespacedKindFor(gr)
		if err != nil {
			fldErr := field.Invalid(fldPath, gr, err.Error())
			allErrs = append(allErrs, fldErr)
		}

		// Validate if the configuration of a type already exists. Each type should
		// only have one configuration.
		if _, exists := ts[gvk]; exists {
			fldErr := field.Duplicate(fldPath, gr)
			allErrs = append(allErrs, fldErr)
		}
		ts[gvk] = r.Mode
	}
	if len(allErrs) > 0 {
		return apierrors.NewInvalid(api.HNCConfigurationGK, api.HNCConfigSingleton, allErrs)
	}
	return nil
}

func (v *Validator) checkForest(ts gvkSet) error {
	v.Forest.Lock()
	defer v.Forest.Unlock()

	// Get types that are changed from other modes to "Propagate" mode or "AllowPropagate" mode.
	gvks := v.getNewPropagateTypes(ts)

	// Check if user-created objects would be overwritten by these mode changes.
	for gvk, mode := range gvks {
		conflicts := v.checkConflictsForGVK(gvk, mode)
		if len(conflicts) != 0 {
			msg := fmt.Sprintf("Cannot update configuration because setting type %q to 'Propagate' mode or 'AllowPropagate' mode would overwrite user-created object(s):\n", gvk)
			msg += strings.Join(conflicts, "\n")
			msg += "\nTo fix this, please rename or remove the conflicting objects first."
			err := errors.New(msg)
			// TODO(erikgb): Invalid field error better?
			return apierrors.NewConflict(api.HNCConfigurationGR, api.Singleton, err)
		}
	}

	return nil
}

// checkConflictsForGVK looks for conflicts from top down for each tree.
func (v *Validator) checkConflictsForGVK(gvk schema.GroupVersionKind, mode api.SynchronizationMode) []string {
	conflicts := []string{}
	for _, ns := range v.Forest.GetRoots() {
		conflicts = append(conflicts, v.checkConflictsForTree(gvk, ancestorObjects{}, ns, mode)...)
	}
	return conflicts
}

// checkConflictsForTree check for all the gvk objects in the given namespaces, to see if they
// will be potentially overwritten by the objects on the ancestor namespaces
func (v *Validator) checkConflictsForTree(gvk schema.GroupVersionKind, ao ancestorObjects, ns *forest.Namespace, mode api.SynchronizationMode) []string {
	conflicts := []string{}
	// make a local copy of the ancestorObjects so that the original copy doesn't get modified
	objs := ao.copy()
	for _, srcNNM := range ns.GetSourceNames(gvk) {
		onm := srcNNM.Name
		// If there exists objects with the same name and gvk, check if there will be overwriting conflict
		for _, nnm := range objs[onm] {
			// check if the existing ns will propagate this object to the current ns
			inst := v.Forest.Get(nnm).GetSourceObject(gvk, onm)
			if ok, _ := selectors.ShouldPropagate(inst, ns.GetLabels(), mode); ok {
				conflicts = append(conflicts, fmt.Sprintf("  Object %q in namespace %q would overwrite the one in %q", onm, nnm, ns.Name()))
			}
		}
		objs.add(onm, ns)
	}
	// This is cycle-free and safe because we only start the
	// "checkConflictsForTree" from roots in the forest with cycles omitted and
	// it's impossible to get cycles from non-root.
	for _, cnm := range ns.ChildNames() {
		cns := v.Forest.Get(cnm)
		conflicts = append(conflicts, v.checkConflictsForTree(gvk, objs, cns, mode)...)
	}
	return conflicts
}

// getNewPropagateTypes returns a set of types that are changed from other modes
// to `Propagate` or `AllowPropagate` mode.
func (v *Validator) getNewPropagateTypes(ts gvkSet) gvkSet {
	// Get all "Propagate" mode and "AllowPropagate" mode types in the new configuration.
	newPts := gvkSet{}
	for gvk, mode := range ts {
		if mode == api.Propagate {
			newPts[gvk] = api.Propagate
		}
		if mode == api.AllowPropagate {
			newPts[gvk] = api.AllowPropagate
		}
	}

	// Remove all existing "Propagate" mode and "AllowPropagate" mode types in the forest (current configuration).
	for _, t := range v.Forest.GetTypeSyncers() {
		_, exist := newPts[t.GetGVK()]
		if t.CanPropagate() && exist {
			delete(newPts, t.GetGVK())
		}
	}

	return newPts
}

// ancestorObjects maps an object name to the ancestor namespace(s) in which
// it's defined.
type ancestorObjects map[string][]string

func (a ancestorObjects) copy() ancestorObjects {
	objs := ancestorObjects{}
	for k, v := range a {
		objs[k] = v
	}
	return objs
}

func (a ancestorObjects) add(onm string, ns *forest.Namespace) {
	a[onm] = append(a[onm], ns.Name())
}

func (v *Validator) SetupWithManager(mgr ctrl.Manager) error {
	mapper, err := apimeta.NewGroupKindMapper(mgr.GetConfig())
	if err != nil {
		return err
	}
	v.mapper = mapper
	return nil
}
