package hrq

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/hrq/utils"
	"sigs.k8s.io/hierarchical-namespaces/internal/logutils"
	"sigs.k8s.io/hierarchical-namespaces/internal/metadata"
)

// HRQEnqueuer enqueues HierarchicalResourceQuota objects.
// HierarchicalResourceQuotaReconciler implements the interface so that it can
// be called to update HierarchicalResourceQuota objects.
type HRQEnqueuer interface {
	// Enqueue enqueues HierarchicalResourceQuota objects of the given names in
	// the give namespaces.
	Enqueue(log logr.Logger, reason, nsnm, qnm string)
}

// ResourceQuotaReconciler reconciles RQs per namespace, which represents the HRQ in this
// and any ancestor namespaces. The reconciler is called on two occasions:
//  1. The HRQ in this or an ancestor namespace has changed. This can either be because an HRQ has
//     been modified (in which case, the HRQR will call EnqueueSubtree) or because the ancestors of
//     this namespace have changed (in which case, the NSR will call OnChangeNamespace). Either way,
//     this will typically result in the limits being updated.
//  2. The K8s apiserver has modified the usage of this RQ, typically in response to a resource being
//     _released_ but it's also possible to observe increasing usage here as well (see go/hnc-hrq for
//     details). In such cases, we basically just need to enqueue the HRQs in all ancestor namespaces
//     so that they can update their usages as well.
type ResourceQuotaReconciler struct {
	client.Client
	eventRecorder record.EventRecorder
	Log           logr.Logger

	// Forest is the in-memory data structure that is shared with all other reconcilers.
	Forest *forest.Forest

	// trigger is a channel of event.GenericEvent (see "Watching Channels" in
	// https://book-v1.book.kubebuilder.io/beyond_basics/controller_watches.html)
	// that is used to enqueue the singleton to trigger reconciliation.
	trigger chan event.GenericEvent
	HRQR    HRQEnqueuer
}

// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete

func (r *ResourceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// The reconciler only reconciles ResourceQuota objects created by itself.
	if !strings.Contains(req.NamespacedName.Name, api.ResourceQuotaSingletonName) {
		return ctrl.Result{}, nil
	}

	namespaceName := req.NamespacedName.Namespace
	name := req.NamespacedName.Name
	log := logutils.WithRID(r.Log).WithValues("trigger", req.NamespacedName)

	var notFound bool
	inst, err := r.getRQ(ctx, namespaceName, name)
	if apierrors.IsNotFound(err) {
		notFound = true
		inst = &v1.ResourceQuota{}
		inst.ObjectMeta.Name = name
		inst.ObjectMeta.Namespace = namespaceName
	} else if err != nil {
		log.Error(err, "Couldn't read singleton")
		return ctrl.Result{}, err
	}

	isSingleton := utils.IsSingletonRQ(inst)

	r.Forest.Lock()
	ns := r.Forest.Get(inst.ObjectMeta.Namespace)
	_, ok := ns.GetQuota(name)
	if !ok {
		ns.SetQuota(name)
	}
	r.Forest.Unlock()

	// Update our limits, and enqueue any related HRQs if our usage has changed.
	updated, err := r.syncWithForest(log, inst)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Reconciling ResourceQuota", "name", fmt.Sprintf("%s/%s", inst.GetNamespace(), inst.GetName()), "limits", inst.Spec.Hard, "usages", inst.Status.Used, "updated", updated)

	// Delete the obsolete singleton and early exit if the new limits are empty.
	if inst.Spec.Hard == nil {
		return ctrl.Result{}, r.deleteRQ(ctx, log, inst)
	} else if !isSingleton && notFound {
		hrq := &api.HierarchicalResourceQuota{}
		hrqName, err := utils.ScopedHRQNameFromHRQName(inst.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("while getting hrq name: %w", err)
		}

		cursorNm := ns
		var found bool
		for {
			if cursorNm == nil {
				break
			}

			hrqnnm := types.NamespacedName{Namespace: cursorNm.Name(), Name: hrqName}
			err := r.Get(ctx, hrqnnm, hrq)
			if err == nil {
				found = true
				break
			}
			if apierrors.IsNotFound(err) {
				cursorNm = cursorNm.Parent()
				continue
			}

			return ctrl.Result{}, fmt.Errorf("while getting hrq: %w", err)
		}
		if !found {
			return ctrl.Result{}, fmt.Errorf("the parent hrq not found: %s", hrqName)
		}

		log.Info("Found the parent HRQ", "namespace", hrq.Namespace, "name", hrq.Name)

		inst.Spec.ScopeSelector = hrq.Spec.ScopeSelector
	}

	// We only need to write back to the apiserver if the spec has changed
	if updated || notFound {
		if err := r.writeRQ(ctx, log, inst); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// getRQ returns the RQ object if it exists.
func (r *ResourceQuotaReconciler) getRQ(ctx context.Context, ns, name string) (*v1.ResourceQuota, error) {
	nnm := types.NamespacedName{Namespace: ns, Name: name}
	inst := &v1.ResourceQuota{}
	if err := r.Get(ctx, nnm, inst); err != nil {
		return nil, err
	}

	return inst, nil
}

// writeRQ creates a RQ object on the apiserver if it does not exist.
// Otherwise, it updates existing RQ object on the apiserver.
func (r *ResourceQuotaReconciler) writeRQ(ctx context.Context, log logr.Logger, inst *v1.ResourceQuota) error {
	if inst.CreationTimestamp.IsZero() {
		// Set the cleanup label so the singleton can be deleted by selector later.
		metadata.SetLabel(inst, api.HRQLabelCleanup, "true")
		// Add a non-propagate exception annotation to the instance so that it won't
		// be overwritten by ancestors, when the resource quota type is configured
		// as Propagate mode in HNCConfig.
		metadata.SetAnnotation(inst, api.NonPropagateAnnotation, "true")
		log.V(1).Info("Creating a RQ on apiserver", "limits", inst.Spec.Hard)
		if err := r.Create(ctx, inst); err != nil {
			r.reportHRQEvent(ctx, log, inst, err)
			return fmt.Errorf("while creating rq: %w", err)
		}
		return nil
	}

	log.V(1).Info("Updating the RQ on apiserver", "oldLimits", inst.Status.Hard, "newLimits", inst.Spec.Hard)
	if err := r.Update(ctx, inst); err != nil {
		r.reportHRQEvent(ctx, log, inst, err)
		return fmt.Errorf("while updating rq: %w", err)
	}
	return nil
}

// reportHRQEvent reports events on all ancestor HRQs if the error is not nil.
func (r *ResourceQuotaReconciler) reportHRQEvent(ctx context.Context, log logr.Logger, inst *v1.ResourceQuota, err error) {
	if err == nil {
		return
	}

	for _, nnm := range r.getAncestorHRQs(inst) {
		hrq := &api.HierarchicalResourceQuota{}
		// Since the Event() func requires the full object as an input, we will
		// have to get the HRQ from the apiserver here.
		if err := r.Get(ctx, nnm, hrq); err != nil {
			// We just log the error and continue here because this function is only
			// called when the reconciler already decides to return an error to retry.
			log.Error(err, "While trying to generate an event on the instance")
			continue
		}
		var errMsg string
		if err != nil {
			errMsg = ignoreRQErr(err.Error())
		}
		msg := fmt.Sprintf("could not create/update lower-level ResourceQuota %s: %s", nnm, errMsg)
		r.eventRecorder.Event(hrq, "Warning", api.EventCannotWriteResourceQuota, msg)
	}
}

// There may be race condition here since we previously release the hold that the
// forest may have changed. However, it should be fine since the caller uses the
// result to generate events on and will return an error for this reconciler to
// retry. We will eventually get the right ancestor HRQs.
func (r *ResourceQuotaReconciler) getAncestorHRQs(inst *v1.ResourceQuota) []types.NamespacedName {
	r.Forest.Lock()
	defer r.Forest.Unlock()

	names := []types.NamespacedName{}
	for _, nsnm := range r.Forest.Get(inst.Namespace).AncestryNames() {
		for _, hrqnm := range r.Forest.Get(nsnm).HRQNames() {
			names = append(names, types.NamespacedName{Namespace: nsnm, Name: hrqnm})
		}
	}

	return names
}

// deleteRQ deletes a resource quota on the apiserver and a quota in on-memory if it exists. Otherwise,
// do nothing.
func (r *ResourceQuotaReconciler) deleteRQ(ctx context.Context, log logr.Logger, inst *v1.ResourceQuota) error {
	// Early exit if the singleton doesn't already exist.
	if inst.CreationTimestamp.IsZero() {
		return nil
	}

	log.V(1).Info("Deleting obsolete empty RQ on apiserver")
	if err := r.Delete(ctx, inst); err != nil {
		return fmt.Errorf("while deleting rq: %w", err)
	}

	r.Forest.Get(inst.Namespace).RemoveQuota(inst.Name)

	return nil
}

// syncWithForest syncs limits and resource usages of in-memory `hrq` objects of
// current namespace and its ancestors with the ResourceQuota object of the
// namespace. Specifically, it performs following tasks:
//   - Syncs `ResourceQuota.Spec.Hard` with `hrq.hard` of the current namespace
//     and its ancestors.
//   - Updates `hrq.used.local` of the current namespace and `hrq.used.subtree`
//     of the current namespace and its ancestors based on `ResourceQuota.Status.Used`.
func (r *ResourceQuotaReconciler) syncWithForest(log logr.Logger, inst *v1.ResourceQuota) (bool, error) {
	r.Forest.Lock()
	defer r.Forest.Unlock()
	ns := r.Forest.Get(inst.ObjectMeta.Namespace)

	// Determine if the RQ's spec needs to be updated
	rqName := inst.GetName()

	updated := r.syncResourceLimits(ns, inst, rqName)

	// Since all resourcequota usage changes will be caught by this reconciler (no
	// matter it's from K8s resourcequota admission controller or K8s resourcequota
	// controller), we consolidate all the affected HRQ enqueuing here.
	//
	// Note that we say UseResources, and not TryUseResources, which means that even if we're over
	// quota, the resource counts will be still be increased. This is because by the time the
	// reconciler's running, the resources truly have been consumed so we just need to (accurately)
	// reflect that we're over quota.
	usage, err := ns.GetLocalUsages(inst.Name)
	if errors.Is(err, forest.ErrQuotaNotFound) {
		usage = v1.ResourceList{}
		updated = true
	} else if err != nil {
		return false, fmt.Errorf("while getting local usages: %w", err)
	}

	log.V(1).Info("RQ usages may have updated", "oldUsages", usage, "newUsages", inst.Status.Used)

	if err := ns.UseResources(rqName, inst.Status.Used); err != nil {
		return false, fmt.Errorf("while using resources: %w", err)
	}

	for _, nsnm := range ns.AncestryNames() {
		for _, qnm := range r.Forest.Get(nsnm).HRQNames() {
			r.HRQR.Enqueue(log, "subtree resource usages may have changed", nsnm, qnm)
		}
	}

	return updated, nil
}

// syncResourceLimits updates `ResourceQuota.Spec.Hard` to be the union types from of `hrq.hard` of
// the namespace and its ancestors. If there are more than one limits for a resource type in the
// union of `hrq.hard`, only the most strictest limit will be set to `ResourceQuota.Spec.Hard`.
//
// Returns true if any changes were made, false otherwise.
func (r *ResourceQuotaReconciler) syncResourceLimits(ns *forest.Namespace, inst *v1.ResourceQuota, rqName string) bool {
	// Get the list of all resources that need to be restricted in this namespace, as well as the
	// maximum possible limit for each resource.
	l := ns.Limits(rqName)

	// Check to see if there's been any change and update if so.
	if utils.Equals(l, inst.Spec.Hard) {
		return false
	}
	inst.Spec.Hard = l
	return true
}

// OnChangeNamespace enqueues the singleton in a specific namespace to trigger the reconciliation of
// the singleton for a given reason .  This occurs in a goroutine so the caller doesn't block; since
// the reconciler is never garbage-collected, this is safe.
func (r *ResourceQuotaReconciler) OnChangeNamespace(log logr.Logger, ns *forest.Namespace) {
	for _, nm := range ns.RQNames() {
		r.OnChangeNamespaceWithRQName(log, ns, nm)
	}
}

func (r *ResourceQuotaReconciler) OnChangeNamespaceWithRQName(log logr.Logger, ns *forest.Namespace, name string) {
	nsnm := ns.Name()
	go func() {
		// The watch handler doesn't care about anything except the metadata.
		inst := &v1.ResourceQuota{}
		inst.ObjectMeta.Name = name
		inst.ObjectMeta.Namespace = nsnm
		r.trigger <- event.GenericEvent{Object: inst}
	}()
}

// EnqueueSubtree enqueues ResourceQuota objects of the given namespace and its descendants.
//
// The method is robust against race conditions. The method holds the forest lock so that the
// in-memory forest (specifically the descendants of the namespace that we record in-memory) cannot
// be changed while enqueueing ResourceQuota objects in the namespace and its descendants.
//
// If a new namespace becomes a descendant just after we acquire the lock, the ResourceQuota object
// in the new namespace will be enqueued by the NamespaceReconciler, instead of the
// ResourceQuotaReconciler. By contrast, if a namespace is *removed* as a descendant, we'll still
// call the reconciler but it will have no effect (reconcilers can safely be called multiple times,
// even if the object has been deleted).
func (r *ResourceQuotaReconciler) EnqueueSubtree(log logr.Logger, nsnm, name string, deleted bool) {
	r.Forest.Lock()
	defer r.Forest.Unlock()

	nsnms := r.Forest.Get(nsnm).DescendantNames()
	nsnms = append(nsnms, nsnm)
	for _, nsnm := range nsnms {
		ns := r.Forest.Get(nsnm)
		if !deleted {
			if _, ok := ns.GetQuota(name); !ok {
				ns.SetQuota(name)
			}
		}
		r.OnChangeNamespaceWithRQName(log, ns, name)
	}
}

func (r *ResourceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// This field will be shown as source.component=hnc.x-k8s.io in events.
	r.eventRecorder = mgr.GetEventRecorderFor(api.MetaGroup)
	r.trigger = make(chan event.GenericEvent)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ResourceQuota{}).
		Watches(&source.Channel{Source: r.trigger}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
