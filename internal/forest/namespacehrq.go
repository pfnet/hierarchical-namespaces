package forest

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/hrq/utils"
)

var ErrQuotaNotFound = fmt.Errorf("quota not found")

type quotas struct {
	// limits stores the resource limits specified by the HRQs in this namespace
	limits limits

	// used stores the resource usage in the subtree rooted in this namespace
	used usage
}

// limits maps from the name of the HRQ to the limits it specifies
type limits map[string]v1.ResourceList

// usage stores local and subtree resource usages of a namespace.
type usage struct {
	// local stores resource usages in the namespace. The resource types in `local`
	// is a union of types specified in quotas.limits of the namespace and its ancestors.
	local v1.ResourceList

	// subtree stores the aggregate resource usages in the namespace and its descendants.
	// The resource types in `subtree` are the types specified in `local` usages
	// of the namespace and its descendants.
	//
	// We need to keep track of all types in `local` usages so that when adding a
	// new limit for a type that exists only in HierarchicalResourceQuota objects
	// in descendants of the current namespace, we still have aggregate resource
	// usage for the type.
	subtree v1.ResourceList
}

// TryUseResources checks resource limits in the namespace and its ancestors when given proposed
// absolute (not delta) resource usages in the namespace. If there are any changes in the usages, we
// only check to see if any proposed increases take us over any limits. If any of them exceed
// resource limits, it returns an error suitable to display to end users; otherwise, it updates the
// in-memory usages of both this namespace as well as all its ancestors. Callers of this method are
// responsible for updating resource usage status of the HierarchicalResourceQuota objects.
//
// TryUseResources is called by the HRQ admission controller to decide if a ResourceQuota.Status
// update issued by the K8s ResourceQuota admission controller is allowed. Since UseResources()
// modifies resource usages in the in-memory forest, the forest lock should be held while calling
// the method.
//
// Normally, admission controllers shouldn't assume that if they allow a change, that this change
// will actually be performed, since another admission controller can be called later and deny it.
// Uniquely for Resource Quotas, this isn't true - the K8s apiserver only attempts to update the RQ
// status when _all_ other admission controllers have passed and the resources are about to be
// consumed. In rare cases, of course, the resources may not be consumed (e.g. due to an error in
// etcd) but the apiserver runs a cleanup process that occasionally syncs up actual usage with the
// usage recorded in RQs. When the RQs are changed, we'll be updated too.
//
// Based on observations, the K8s ResourceQuota admission controller is called only when a resource
// is consumed, not when a resource is released. Therefore, in most cases, the proposed resource
// usages that the HRQ admission controller received should be larger than in-memory resource
// usages. However, this function is robust to (that is, always allows) decreases as well, mainly
// because it's easier to test - plus, who knows, the K8s behaviour may change in the future.
//
// This may allow one weird case where a user may be allowed to use something they weren't supposed
// to. Let's say you're well over your limit, and then in quick succession, some resources are
// deleted, and some _fewer_ are added, but enough to still go over the limit. In that case, there's
// a race condition between this function being called, and the RQ reconciler updating the baseline
// resource usage. If this function wins, it will look like resource usage is decreasing, and will
// be incorrectly allowed. If the RQ reconciler runs first, we'll see that the usage is incorrectly
// _increasing_ and it will be disallowed. However, I think the simplicity of not trying to prevent
// this (hopefully very unlikely) corner case is more valuable than trying to catch it.
func (n *Namespace) TryUseResources(rl v1.ResourceList, rqName RQName) error {
	if err := n.canUseResources(rl, rqName); err != nil {
		// At least one of the proposed usage exceeds resource limits.
		return err
	}

	// At this point we are confident that no proposed resource usage exceeds
	// resource limits because the forest lock is held by the caller of this method.
	if err := n.UseResources(rqName, rl); err != nil {
		return err
	}

	return nil
}

// canUseResources checks if subtree resource usages exceed resource limits
// in the namespace and its ancestors if proposed resource usages were consumed.
// The method returns an error if any *changing* subtree resource usages exceed
// the corresponding limits; otherwise, it returns nil. Note: if there's no
// *change* on a subtree resource usage and it already exceeds limits, we will
// ignore it because we don't want to block other valid resource usages.
func (n *Namespace) canUseResources(u v1.ResourceList, rqName RQName) error {
	quota, ok := n.GetQuota(rqName)
	if !ok {
		quota = &quotas{}
	}

	// For each resource, delta = proposed usage - current usage.
	delta := utils.Subtract(u, quota.used.local)
	// Only consider *increasing* deltas; see comments to TryUseResources for details.
	increases := utils.OmitLTEZero(delta)

	for _, nsnm := range n.AncestryNames() {
		ns := n.forest.Get(nsnm)
		nsQuota, ok := ns.quotas[rqName]
		if !ok {
			continue
		}

		// Use AddIfExists (not Add) because we want to ignore any resources that aren't increasing when
		// checking against the limits.
		proposed := utils.AddIfExists(increases, nsQuota.used.subtree)
		allowed, nm, exceeded := checkLimits(nsQuota.limits, proposed)
		if allowed {
			continue
		}

		// Construct the error message similar to the RQ exceeded quota error message -
		// "exceeded quota: gke-hc-hrq, requested: configmaps=1, used: configmaps=2, limited: configmaps=2"
		msg := fmt.Sprintf("exceeded hierarchical quota in namespace %q: %q", ns.name, nm)
		for _, er := range exceeded {
			rnm := er.String()
			// Get the requested, used, limited quantity of the exceeded resource.
			rq := increases[er]
			uq := nsQuota.used.subtree[er]
			lq := nsQuota.limits[nm][er]
			msg += fmt.Sprintf(", requested: %s=%v, used: %s=%v, limited: %s=%v",
				rnm, &rq, rnm, &uq, rnm, &lq)
		}
		return fmt.Errorf(msg)
	}

	return nil
}

// UseResources sets the absolute resource usage in this namespace, and should
// be called when we're being informed of a new set of resource usage. It also
// updates the subtree usage in this namespace and all its ancestors.
//
// The callers will typically then enqueue all ancestor HRQs to update their
// usages with apiserver.
//
// UseResources can be called in the following scenarios:
//   - Called by the HRQ admission controller when a request is allowed
//   - Called by the HRQ ResourceQuota reconciler when it observes `local`
//     usages are different from ResourceQuota.Status.Used
//   - Called by the HRQ Namespace reconciler to remove `local` usages of a
//     namespace from the subtree usages of the previous ancestors of the namespace.
//   - Called by the SetParent to remove `local` usages of a namespace from
//     the subtree usages of the previous ancestors of the namespace and add the
//     usages to the new ancestors following a parent update
func (n *Namespace) UseResources(rqName string, newUsage v1.ResourceList) error {
	quota, ok := n.GetQuota(rqName)
	if !ok {
		return fmt.Errorf("while using resources: %w", ErrQuotaNotFound)
	}
	oldUsage := quota.used.local

	// We only store the usages we care about
	l := n.Limits(rqName)
	newUsage = utils.FilterUnlimited(newUsage, l)

	// Early exit if there's no usages change. It's safe because the forest would
	// remain unchanged and the caller would always enqueue all ancestor HRQs.
	if utils.Equals(oldUsage, newUsage) {
		return nil
	}
	quota.used.local = newUsage

	// Determine the delta in resource usage as this now needs to be applied to each ancestor.
	delta := utils.Subtract(newUsage, oldUsage)

	// Update subtree usages in the ancestors (including itself). The incremental
	// change here is safe because there's a goroutine periodically calculating
	// subtree usages from-scratch to make sure the forest is not out-of-sync. If
	// all goes well, the periodic sync isn't needed - it's *purely* there in case
	// there's a bug.
	for _, nsnm := range n.AncestryNames() {
		ns := n.forest.Get(nsnm)

		nsQuota, ok := ns.quotas[rqName]
		if !ok {
			return fmt.Errorf("no quota found for %q", rqName)
		}

		// Get the new subtree usage and remove no longer limited usages.
		newSubUsg := utils.Add(delta, nsQuota.used.subtree)
		l := ns.Limits(rqName)
		nsQuota.used.subtree = utils.FilterUnlimited(newSubUsg, l)
	}

	return nil
}

// checkLimits checks if resource usages exceed resource limits specified in
// HierarchicalResourceQuota objects of a namespace. If resource usages exceed
// resource limits in a HierarchicalResourceQuota object, it will return false, the
// name of the HierarchicalResourceQuota Object that defines the resource limit, and
// the name(s) of the resource(s) that exceed the limits; otherwise, it will return
// true.
func checkLimits(l limits, u v1.ResourceList) (bool, string, []v1.ResourceName) {
	for nm, rl := range l {
		if allowed, exceeded := utils.LessThanOrEqual(u, rl); !allowed {
			return allowed, nm, exceeded
		}
	}
	return true, "", nil
}

// HRQNames returns the names of every HRQ object in this namespace
func (n *Namespace) HRQNames() []string {
	names := []string{}
	for _, quota := range n.quotas {
		for nm := range quota.limits {
			names = append(names, nm)
		}
	}
	return names
}

func (n *Namespace) RQNames() []string {
	rqNames := []string{}
	for rqName := range n.quotas {
		rqNames = append(rqNames, rqName)
	}
	return rqNames
}

// Limits returns limits limits specified in quotas.limits of the current namespace and
// its ancestors. If there are more than one limits for a resource type, the
// most strictest limit will be returned.
func (n *Namespace) Limits(rqName string) v1.ResourceList {
	rs := v1.ResourceList{}
	for _, nsnm := range n.AncestryNames() {
		ns := n.forest.Get(nsnm)
		quota, ok := ns.GetQuota(rqName)
		if !ok {
			continue
		}

		for _, l := range quota.limits {
			rs = utils.Min(rs, l)
		}
	}

	return rs
}

// GetLocalUsages returns a copy of local resource usages.
func (n *Namespace) GetLocalUsages(rqName string) (v1.ResourceList, error) {
	quota, ok := n.quotas[rqName]
	if !ok {
		return nil, ErrQuotaNotFound
	}
	u := quota.used.local.DeepCopy()
	return u, nil
}

// GetSubtreeUsages returns a copy of subtree resource usages.
func (n *Namespace) GetSubtreeUsages(rqName string) (v1.ResourceList, error) {
	quota, ok := n.quotas[rqName]
	if !ok {
		return nil, ErrQuotaNotFound
	}
	u := quota.used.subtree.DeepCopy()
	return u, nil
}

// TestOnlySetSubtreeUsage overwrites the actual, calculated subtree usages and replaces them with
// arbitrary garbage. Needless to say, you should never call this, unless you're testing HNC's
// ability to recover from arbitrary garbage.
//
// The passed-in arg is used as-is, not copied. This is test code, so deal with it 😎
func (n *Namespace) TestOnlySetSubtreeUsage(rl v1.ResourceList, rqName string) error {
	quota, ok := n.GetQuota(rqName)
	if !ok {
		return ErrQuotaNotFound
	}
	quota.used.subtree = rl
	return nil
}

// RemoveLimits removes limits specified by the HierarchicalResourceQuota object
// of the given name.
func (n *Namespace) RemoveLimits(nm, rqName string) error {
	quota, ok := n.GetQuota(rqName)
	if !ok {
		return nil
	}
	delete(quota.limits, nm)

	return nil
}

// UpdateLimits updates in-memory limits of the HierarchicalResourceQuota
// object of the given name. Returns true if there's a difference.
func (n *Namespace) UpdateLimits(nm, rqName string, l v1.ResourceList) bool {
	quota, ok := n.GetQuota(rqName)
	if !ok {
		quota = n.SetQuota(rqName)
	}

	if quota.limits == nil {
		quota.limits = limits{}
	}
	if utils.Equals(quota.limits[nm], l) {
		return false
	}
	quota.limits[nm] = l
	return true
}

// RectifySubtreeUsages ensures that the subtree usages of every namespaces is in sync with all of
// its descendants. This should be ensured by the logic in UseResources, but bugs happen, so this is
// an added level of safety. If any discrepancies are found, this function logs an error, updates
// the corrected usages in-memory, and returns a list of affected HRQ objects so that they can be
// re-reconciled to show the corrected usages.
//
// The forest lock must be held when calling this function.
func (f *Forest) RectifySubtreeUsages(log logr.Logger) ([]types.NamespacedName, error) {
	// Recalculate all usages for only Singleton from scratch
	usages := map[string]v1.ResourceList{}
	for _, ns := range f.namespaces {
		for rqName, quota := range ns.quotas {
			if rqName != api.ResourceQuotaSingletonName {
				continue
			}

			local := quota.used.local
			// NB: AncestryNames includes the namespace itself
			for _, anc := range ns.AncestryNames() {
				if existing, ok := usages[anc]; ok {
					usages[anc] = utils.Add(existing, local)
				} else {
					usages[anc] = local
				}
			}
		}
	}

	// Look for any out-of-date HRQ usages
	updated := []types.NamespacedName{}
	for nm, ns := range f.namespaces {
		for rqName, quota := range ns.quotas {
			if rqName != api.ResourceQuotaSingletonName {
				continue
			}

			have := quota.used.subtree
			limit := ns.Limits(rqName)

			actual := utils.FilterUnlimited(usages[nm], limit)
			if utils.Equals(have, actual) {
				continue
			}

			// Oopsies.
			err := errors.New("HRQ correctness error")
			log.Error(err, "incrementally calculated usages are incorrect", "ns", nm, "incremental", have, "actual", actual)

			// Update and return info so the reconciler can write back the corrected usages.
			quota.used.subtree = actual
			ns.quotas[rqName] = quota
			for hrq := range quota.limits {
				// These are the names of the actual objects
				updated = append(updated, types.NamespacedName{
					Namespace: nm,
					Name:      hrq,
				})
			}
		}
	}

	return updated, nil
}

func (f *Forest) NamespaceHavingScopedHRQ() map[string][]RQName {
	nss := map[string][]RQName{}
	for _, ns := range f.namespaces {
		nss[ns.Name()] = ns.ScopedRQNames()
	}

	return nss
}

func (f *Forest) CleanupQuotas(log logr.Logger) {
	// TODO:(utam0k): Consider the same name of HRQ in different namespaces.

	log.Info("Cleaning up quotas...")
	cleanupQuotas := map[string][]*Namespace{}
	for _, ns := range f.namespaces {
		for rqName, quota := range ns.quotas {
			if quota.limits == nil {
				nss, ok := cleanupQuotas[rqName]
				if !ok {
					cleanupQuotas[rqName] = []*Namespace{ns}
				} else {
					cleanupQuotas[rqName] = append(nss, ns)
				}
			}
		}
	}

	for _, ns := range f.namespaces {
		for rqName, quota := range ns.quotas {
			if quota.limits != nil {
				delete(cleanupQuotas, rqName)
			}
		}
	}

	for rqName, nss := range cleanupQuotas {
		for _, ns := range nss {
			log.Info("Removed the quota", "name", rqName, "namespace", ns.name)
			ns.RemoveQuota(rqName)
		}
	}
}
