package hierarchyconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/config"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/selectors"
	"sigs.k8s.io/hierarchical-namespaces/internal/webhooks"
)

const (
	// ServingPath is where the validator will run. Must be kept in sync with the
	// kubebuilder marker below.
	ServingPath = "/validate-hnc-x-k8s-io-v1alpha2-hierarchyconfigurations"
)

// Note: the validating webhook FAILS CLOSED. This means that if the webhook goes down, all further
// changes to the hierarchy are forbidden. However, new objects will still be propagated according
// to the existing hierarchy (unless the reconciler is down too).
//
// +kubebuilder:webhook:admissionReviewVersions=v1,path=/validate-hnc-x-k8s-io-v1alpha2-hierarchyconfigurations,mutating=false,failurePolicy=fail,groups="hnc.x-k8s.io",resources=hierarchyconfigurations,sideEffects=None,verbs=create;update,versions=v1alpha2,name=hierarchyconfigurations.hnc.x-k8s.io

type Validator struct {
	Forest *forest.Forest
	server serverClient
}

func NewValidator(forest *forest.Forest, client client.Client) *Validator {
	return &Validator{
		Forest: forest,
		server: &realClient{client: client},
	}
}

// serverClient represents the checks that should typically be performed against the apiserver, but
// need to be stubbed out during unit testing.
type serverClient interface {
	// Exists returns true if the given namespace exists.
	Exists(ctx context.Context, nnm string) (bool, error)

	// IsAdmin takes a UserInfo and the name of a namespace, and returns true if the user is an admin
	// of that namespace (ie, can update the hierarchical config).
	IsAdmin(ctx context.Context, ui *authnv1.UserInfo, nnm string) (bool, error)
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, obj)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, obj)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.handle(ctx, newObj)
}

// handle implements the validation webhook.
//
// During updates, the validator currently ignores the existing state of the object (`oldObject`).
// The reason is that most of the checks being performed are on the state of the entire forest, not
// on any one object, so having the _very_ latest information on _one_ object doesn't really help
// us. That is, we're basically forced to assume that the in-memory forest is fully up-to-date.
//
// Obviously, there are times when this assumption will be incorrect - for example, when the HNC is
// just starting up, or perhaps if there have been a lot of changes made very quickly that the
// reconciler has't caught up with yet. In such cases, this validator can produce both false
// negatives (legal changes are incorrectly rejected) or false positives (illegal changes are
// mistakenly allowed).  False negatives can easily be retried and so are not a significant problem,
// since (by definition) we expect the problem to be transient.
//
// False positives are a more serious concern, and fall into two categories: structural failures,
// and authz failures. Regarding structural failures, the reconciler has been designed to assume
// that the validator is _never_ running, and any illegal configuration that makes it into K8s will
// simply be reported via HierarchyConfiguration.Status.Conditions. It's the admins'
// responsibilities to monitor these conditions and ensure that, transient exceptions aside, all
// namespaces are condition-free. Note that even if the validator is working perfectly, it's still
// possible to introduce structural failures, as described in the user docs.
//
// Authz false positives are prevented as described by the comments to `getServerChecks`.
//
// This follows the standard HNC pattern of:
// - Load a bunch of stuff from the apiserver
// - Lock the forest and do all checks
// - Finish up with the apiserver (although we just run _additional_ checks, we don't modify things)
//
// This minimizes the amount of time that the forest is locked, allowing different threads to
// proceed in parallel.
func (v *Validator) handle(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	log := admission.DefaultLogConstructor(logf.FromContext(ctx), &req)

	// Early exit: the HNC SA can do whatever it wants. This is because if an illegal HC already
	// exists on the K8s server, we need to be able to update its status even though the rest of the
	// object wouldn't pass legality. We should probably only give the HNC SA the ability to modify
	// the _status_, though. TODO: https://github.com/kubernetes-sigs/hierarchical-namespaces/issues/80.
	if webhooks.IsHNCServiceAccount(&req.UserInfo) {
		return nil, nil
	}

	hc, ok := obj.(*api.HierarchyConfiguration)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("expected a HierarchyConfiguration but got a %T", obj))
	}

	if why := config.WhyUnmanaged(hc.Namespace); why != "" {
		return nil, apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("namespace %q is not managed by HNC (%s) and cannot be set as a child of another namespace", hc.Namespace, why))
	}
	if why := config.WhyUnmanaged(hc.Spec.Parent); why != "" {
		return nil, apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("namespace %q is not managed by HNC (%s) and cannot be set as the parent of another namespace", hc.Spec.Parent, why))
	}

	labelErrs := config.ValidateManagedLabels(hc.Spec.Labels)
	annotationErrs := config.ValidateManagedAnnotations(hc.Spec.Annotations)
	allErrs := append(labelErrs, annotationErrs...)
	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(api.HierarchyConfigurationGK, hc.Name, allErrs)
	}

	// Do all checks that require holding the in-memory lock. Generate a list of server checks we
	// should perform once the lock is released.
	serverChecks, err := v.checkForest(hc)
	if err != nil {
		return nil, err
	}

	// Ensure that the server's in the right state to make the changes.
	return nil, v.checkServer(ctx, log, &req.UserInfo, serverChecks)
}

// checkForest validates that the request is allowed based on the current in-memory state of the
// forest. If it is, it returns a list of checks we need to perform against the apiserver in order
// to be allowed to make the change; these checks are executed _after_ the in-memory lock is
// released.
func (v *Validator) checkForest(hc *api.HierarchyConfiguration) ([]serverCheck, error) {
	v.Forest.Lock()
	defer v.Forest.Unlock()

	// Load stuff from the forest
	ns := v.Forest.Get(hc.ObjectMeta.Namespace)
	curParent := ns.Parent()
	newParent := v.Forest.Get(hc.Spec.Parent)

	// Check problems on the namespace itself
	if err := v.checkNS(ns); err != nil {
		return nil, err
	}

	// Check problems on the parents
	if err := v.checkParent(ns, curParent, newParent); err != nil {
		return nil, err
	}

	// The structure looks good. Get the list of namespaces we need server checks on.
	return v.getServerChecks(curParent, newParent), nil
}

// checkNS looks for problems with the current namespace that should prevent changes.
func (v *Validator) checkNS(ns *forest.Namespace) error {
	// Wait until the namespace has been synced
	if !ns.Exists() {
		return apierrors.NewServiceUnavailable(fmt.Sprintf("HNC has not reconciled namespace %q yet - please try again in a few moments.", ns.Name()))
	}

	// Deny the request if the namespace has a halted root - but not if it's halted itself, since we
	// may be trying to resolve the halted condition.
	haltedRoot := ns.GetHaltedRoot()
	if haltedRoot != "" && haltedRoot != ns.Name() {
		return apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("ancestor %q of namespace %q has a critical condition, which must be resolved before any changes can be made to the hierarchy configuration", haltedRoot, ns.Name()))
	}

	return nil
}

// checkParent validates if the parent is legal based on the current in-memory state of the forest.
func (v *Validator) checkParent(ns, curParent, newParent *forest.Namespace) error {
	if ns.IsExternal() && newParent != nil {
		return apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("namespace %q is managed by %q, not HNC, so it cannot have a parent in HNC", ns.Name(), ns.Manager))
	}

	if curParent == newParent {
		return nil
	}

	// Prevent changing parent of a subnamespace
	if ns.IsSub {
		return apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("illegal parent: Cannot set the parent of %q to %q because it's a subnamespace of %q", ns.Name(), newParent.Name(), curParent.Name()))
	}

	// non existence of parent namespace -> not allowed
	if newParent != nil && !newParent.Exists() {
		return apierrors.NewForbidden(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("requested parent %q does not exist", newParent.Name()))
	}

	// Is this change structurally legal? Note that this can "leak" information about the hierarchy
	// since we haven't done our authz checks yet. However, the fact that they've gotten this far
	// means that the user has permission to update the _current_ namespace, which means they also
	// have visibility into its ancestry and descendents, and this check can only fail if the new
	// parent conflicts with something in the _existing_ hierarchy.
	if reason := ns.CanSetParent(newParent); reason != "" {
		return apierrors.NewConflict(api.HierarchyConfigurationGR, api.Singleton, fmt.Errorf("illegal parent: %s", reason))
	}

	// Prevent overwriting source objects in the descendants after the hierarchy change.
	if co := v.getConflictingObjects(newParent, ns); len(co) != 0 {
		msg := "Cannot update hierarchy because it would overwrite the following object(s):\n"
		msg += "  * " + strings.Join(co, "\n  * ") + "\n"
		msg += "To fix this, please rename or remove the conflicting objects first."
		return apierrors.NewConflict(api.HierarchyConfigurationGR, api.Singleton, errors.New(msg))
	}

	return nil
}

// getConflictingObjects returns a list of namespaced objects if there's any conflict.
func (v *Validator) getConflictingObjects(newParent, ns *forest.Namespace) []string {
	// If the new parent is nil,  early exit since it's impossible to introduce
	// new naming conflicts.
	if newParent == nil {
		return nil
	}
	// Traverse all the types with 'Propagate' mode or 'AllowPropogate' mode to find any conflicts.
	conflicts := []string{}
	for _, t := range v.Forest.GetTypeSyncers() {
		if t.CanPropagate() {
			conflicts = append(conflicts, v.getConflictingObjectsOfType(t.GetGVK(), t.GetMode(), newParent, ns)...)
		}
	}
	return conflicts
}

// getConflictingObjectsOfType returns a list of namespaced objects if there's
// any conflict between the new ancestors and the descendants.
func (v *Validator) getConflictingObjectsOfType(gvk schema.GroupVersionKind, mode api.SynchronizationMode, newParent, ns *forest.Namespace) []string {
	// Get all the source objects in the new ancestors that would be propagated
	// into the descendants.
	newAnsSrcObjs := make(map[string]bool)
	for _, nnm := range newParent.GetAncestorSourceNames(gvk, "") {
		// If the user has chosen not to propagate the object to this descendant,
		// then it should not be included in conflict checks
		o := v.Forest.Get(nnm.Namespace).GetSourceObject(gvk, nnm.Name)
		if ok, _ := selectors.ShouldPropagate(o, o.GetLabels(), mode); ok {
			newAnsSrcObjs[nnm.Name] = true
		}
	}

	// Look in the descendants to find if there's any conflict.
	cos := []string{}
	dnses := append(ns.DescendantNames(), ns.Name())
	for _, dns := range dnses {
		for _, nnm := range v.Forest.Get(dns).GetSourceNames(gvk) {
			if newAnsSrcObjs[nnm.Name] {
				co := fmt.Sprintf("Namespace %q: %s (%v)", dns, nnm.Name, gvk)
				cos = append(cos, co)
			}
		}
	}

	return cos
}

type serverCheckType int

const (
	// checkAuthz verifies that the user is an admin of this namespace
	checkAuthz serverCheckType = iota
	// checkMissing verifies that the namespace does *not* exist on the server
	checkMissing
)

// serverCheck represents a check to perform against the apiserver once the forest lock is released.
type serverCheck struct {
	nnm       string          // the namespace the user needs to be authorized to modify
	checkType serverCheckType // the type of check to perform
	reason    string          // the reason we're checking it (for logs and error messages)
}

// getServerChecks returns the server checks we need to perform in order to verify that this change
// is legal. It must be called while the forest lock is held, but the checks will be performed once
// the lock has been released.
//
// The general rule is that the user must be an admin of the most recent common ancestor of the old
// and new parent, if they're both in the same tree. If they're in *different* trees, the user must
// be an admin of the root of the old tree, and of the new parent. See the user guide or design doc
// for the rationale for this choice.
//
// While this is fairly simple in theory, there are two complications: missing parents and cycles.
//
// If this webhook is working correctly, a namespace can never be deliberately assigned to a parent
// that doesn't exist (in Gitops flows, this means that the client is expected to create all
// namespaces before syncing HierarchyConfiguration objects, or at least be able to keep retrying
// until after all namespaces have been created). Therefore, there are only three cases where an
// ancestor might be missing:
//
// 1. The parent has been deleted, and the namespace is orphaned. In this case, we want to allow the
// namespace to be reassigned to another parent (or become a root) to let admins resolve the problem.
// 2. An ancestor has been deleted, but not the parent. This case is handled by checkNS, above.
// 3. HNC is just starting up and the parent hasn't been synced yet, so we can't determine the tree
// root. In these cases, we want to reject the request and ask the user to try again (e.g. HTTP 503 -
// service unavailable).
//
// Since case #2 is already handled, we only need to distinguish between #1 and #3. So if the
// current parent does not exist in the forest, we ask for a `checkMissing` server check instead of
// the normal `checkAuthz`. If the namespace is _actually_ missing on the apiserver, as expected,
// the check will pass, allowing the admin to fix the error; if it's present (which means we just
// haven't synced it yet), we'll fail with a 503, asking the user to try again later.
//
// The other complication is cycles. We don't do anything special to handle cycles here. If there's
// a cycle, the existing ancestor namespace we select as the "root" will be arbitrary. Hopefully the
// admin trying to resolve the cycle has permissions on *all* the namespaces in the cycle. For the
// new parent, perhaps we should ban moving a namespace *to* a tree with a cycle in it, but that's
// harder to implement and seems like it's not worth the effort.
func (v *Validator) getServerChecks(curParent, newParent *forest.Namespace) []serverCheck {
	// No need for any checks if nothing's changing.
	if curParent == newParent {
		// Note that this also catches the case where both are nil
		return nil
	}

	// If this is currently a root and we're moving into a new tree, just check the parent.
	//
	// Exception: if the current parent is unmanaged (e.g. it used to be managed before, but isn't anymore),
	// treat it as though it's currently a root and allow the change.
	if curParent == nil || !config.IsManagedNamespace(curParent.Name()) {
		if newParent != nil { // could be nil if old parane is unmanaged
			return []serverCheck{{nnm: newParent.Name(), reason: "proposed parent", checkType: checkAuthz}}
		}
		return nil
	}

	// If the current parent is missing, ask for a missing check. Note that only the *direct* parent
	// can be missing; if a more distant ancestor was missing, `ns` would have had a critical
	// condition in the ancestors, and would have failed checkNS.
	if !curParent.Exists() {
		return []serverCheck{{nnm: curParent.Name(), reason: "current missing parent", checkType: checkMissing}}
	}

	// If we're making the namespace into a root, just check the old root.
	curChain := curParent.AncestryNames()
	if newParent == nil {
		return []serverCheck{{nnm: curChain[0], reason: "current root ancestor", checkType: checkAuthz}}
	}

	// This namespace has both old and new parents. If they're in different trees, return the old root
	// and new parent.
	newChain := newParent.AncestryNames()
	if curChain[0] != newChain[0] {
		return []serverCheck{
			{nnm: curChain[0], reason: "current root ancestor", checkType: checkAuthz},
			{nnm: newParent.Name(), reason: "proposed parent", checkType: checkAuthz},
		}
	}

	// There's at least one common ancestor; find the most recent one and return it.
	mrca := curChain[0]
	for i := 1; i < len(curChain) && i < len(newChain); i++ {
		if curChain[i] != newChain[i] {
			break
		}
		mrca = curChain[i]
	}
	return []serverCheck{{
		nnm:       mrca,
		reason:    fmt.Sprintf("most recent common ancestor of current parent %q and proposed parent %q", curParent.Name(), newParent.Name()),
		checkType: checkAuthz,
	}}
}

// checkServer executes the list of requested checks.
func (v *Validator) checkServer(ctx context.Context, log logr.Logger, ui *authnv1.UserInfo, reqs []serverCheck) error {
	if v.server == nil {
		return nil // unit test; TODO put in fake
	}

	// TODO: parallelize?
	for _, req := range reqs {
		switch req.checkType {
		case checkMissing:
			log.Info("Checking existence", "object", req.nnm, "reason", req.reason)
			exists, err := v.server.Exists(ctx, req.nnm)
			if err != nil {
				return apierrors.NewInternalError(fmt.Errorf("while checking existance for %q, the %s: %w", req.nnm, req.reason, err))
			}

			if exists {
				return apierrors.NewServiceUnavailable(fmt.Sprintf("HNC has not reconciled namespace %q yet - please try again in a few moments.", req.nnm))
			}

		case checkAuthz:
			log.Info("Checking authz", "object", req.nnm, "reason", req.reason)
			allowed, err := v.server.IsAdmin(ctx, ui, req.nnm)
			if err != nil {
				return apierrors.NewInternalError(fmt.Errorf("while checking authz for %q, the %s: %w", req.nnm, req.reason, err))
			}

			if !allowed {
				return apierrors.NewUnauthorized(fmt.Sprintf("user %s is not authorized to modify the subtree of %s, which is the %s", ui.Username, req.nnm, req.reason))
			}
		}
	}

	return nil
}

// realClient implements serverClient, and is not use during unit tests.
type realClient struct {
	client client.Client
}

// Exists implements serverClient
func (r *realClient) Exists(ctx context.Context, nnm string) (bool, error) {
	nsn := types.NamespacedName{Name: nnm}
	ns := &corev1.Namespace{}
	if err := r.client.Get(ctx, nsn, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsAdmin implements serverClient
func (r *realClient) IsAdmin(ctx context.Context, ui *authnv1.UserInfo, nnm string) (bool, error) {
	// Convert the Extra type
	authzExtra := map[string]authzv1.ExtraValue{}
	for k, v := range ui.Extra {
		authzExtra[k] = (authzv1.ExtraValue)(v)
	}

	// Construct the request
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: nnm,
				Verb:      "update",
				Group:     "hnc.x-k8s.io",
				Version:   "*",
				Resource:  "hierarchyconfigurations",
			},
			User:   ui.Username,
			Groups: ui.Groups,
			UID:    ui.UID,
			Extra:  authzExtra,
		},
	}

	// Call the server
	err := r.client.Create(ctx, sar)

	// Extract the interesting result
	return sar.Status.Allowed, err
}
