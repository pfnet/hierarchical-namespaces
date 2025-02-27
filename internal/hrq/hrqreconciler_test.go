package hrq_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/hrq/utils"
	. "sigs.k8s.io/hierarchical-namespaces/internal/integtest"
)

func TestHRQReconciler(t *testing.T) {
	HNCRun(t, "HRQ Suite")
}

var _ = BeforeSuite(HNCBeforeSuite)
var _ = AfterSuite(HNCAfterSuite)

const (
	fooHRQName = "foo-quota"
	barHRQName = "bar-quota"
	bazHRQName = "baz-quota"
	rqName     = "hrq.hnc.x-k8s.io"
)

var _ = Describe("HRQ reconciler tests", func() {
	ctx := context.Background()

	var (
		fooName string
		barName string
		bazName string
	)

	highPrioritySelector := v1.ScopeSelector{
		MatchExpressions: []v1.ScopedResourceSelectorRequirement{
			{
				Operator:  v1.ScopeSelectorOpIn,
				ScopeName: "PriorityClass",
				Values:    []string{"high"},
			},
		},
	}

	BeforeEach(func() {
		fooName = CreateNS(ctx, "foo")
		barName = CreateNS(ctx, "bar")
		bazName = CreateNS(ctx, "baz")

		barHier := NewHierarchy(barName)
		barHier.Spec.Parent = fooName
		UpdateHierarchy(ctx, barHier)

		bazHier := NewHierarchy(bazName)
		bazHier.Spec.Parent = fooName
		UpdateHierarchy(ctx, bazHier)

		Eventually(HasChild(ctx, fooName, barName)).Should(Equal(true))
		Eventually(HasChild(ctx, fooName, bazName)).Should(Equal(true))
	})

	AfterEach(func() {
		cleanupHRQObjects(ctx)
	})

	It("should set limits in status correctly after creating an HRQ object", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")

		Eventually(getHRQStatus(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "6", "pods", "3"))
	})

	It("should update limits in status correctly after updating limits in spec", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")

		Eventually(getHRQStatus(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "6", "pods", "3"))

		// Change limits for secrets from 6 to 50
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "50", "pods", "3")

		Eventually(getHRQStatus(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "50", "pods", "3"))
	})

	It("should update usages in status correctly after consuming a resource", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")
		setHRQ(ctx, barHRQName, barName, nil, "secrets", "100", "cpu", "50")
		setHRQ(ctx, bazHRQName, bazName, nil, "pods", "1")
		// Simulate the K8s ResourceQuota controller to update usages.
		updateRQUsage(ctx, fooName, api.ResourceQuotaSingletonName, "secrets", "0", "pods", "0")
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "0", "cpu", "0", "pods", "0")
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "secrets", "0", "pods", "0")

		// Increase secret counts from 0 to 10 in baz and verify that the usage is
		// increased in foo's HRQ but not bar's (not an ancestor of baz) or baz'
		// (secrets is not limited in baz' HRQ).
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "secrets", "10")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "10", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "0", "cpu", "0"))
		Eventually(getHRQUsed(ctx, bazName, bazHRQName)).Should(equalRL("pods", "0"))

		// Increase secret counts from 10 to 11 in baz and verify that the usage is
		// increased in foo's HRQ. bar's (not an ancestor of baz) and baz' (secrets
		// is not limited in baz' HRQ) remain unchanged.
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "secrets", "11")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "11", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "0", "cpu", "0"))
		Eventually(getHRQUsed(ctx, bazName, bazHRQName)).Should(equalRL("pods", "0"))

		// Decrease secret counts from 10 to 0 in baz and ensure all usages are gone.
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "secrets", "0")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "0", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "0", "cpu", "0"))
		Eventually(getHRQUsed(ctx, bazName, bazHRQName)).Should(equalRL("pods", "0"))
	})

	It("should update limits in ResourceQuota objects correctly after deleting an HRQ object", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")
		setHRQ(ctx, barHRQName, barName, nil, "secrets", "100", "cpu", "50")

		// The RQ in foo should equal its HRQ, while in bar it should be the intersection
		Eventually(getRQHard(ctx, fooName, api.ResourceQuotaSingletonName)).Should(equalRL("secrets", "6", "pods", "3"))
		Eventually(getRQHard(ctx, barName, api.ResourceQuotaSingletonName)).Should(equalRL("secrets", "6", "pods", "3", "cpu", "50"))

		// After deleting foo's HRQ, bar's RQ should be the same as its HRQ on its own
		deleteHierarchicalResourceQuota(ctx, fooName, fooHRQName)
		Eventually(getRQHard(ctx, fooName, api.ResourceQuotaSingletonName)).Should(equalRL())
		Eventually(getRQHard(ctx, barName, api.ResourceQuotaSingletonName)).Should(equalRL("secrets", "100", "cpu", "50"))
	})

	It("should recover if the subtree usages are out of sync in the forest and in reality", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "100")
		setHRQ(ctx, barHRQName, barName, nil, "secrets", "100")
		setHRQ(ctx, "hrq-selector", fooName, &highPrioritySelector, "cpu", "4", "pods", "2")

		// Consume 10 secrets in bar and verified that they're used
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "10")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "10"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "10"))

		// If we do the check now, it shouldn't find anything
		drift, err := TestCheckHRQDrift()
		Expect(err).NotTo(HaveOccurred())
		Expect(drift).Should(BeFalse())

		// Scoped HRQs don't affect the result of TestCheckHRQDrift.
		forestOverrideSubtreeUsages("hrq-selector", "cpu", "3")
		updateRQUsage(ctx, fooName, utils.ScopedRQName("hrq-selector"), "cpu", "5")
		drift, err = TestCheckHRQDrift()
		Expect(err).NotTo(HaveOccurred())
		Expect(drift).Should(BeFalse())

		// Introduce a bug to make the foo subtree usages in the forest out-of-sync. Pretend that we
		// somehow dropped a secret.
		forestOverrideSubtreeUsages(fooName, "secrets", "9")

		// Now use an additional secret in the child. The parent should still be out-of-sync by one.
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "11")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "10"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "11"))

		// Run the checker and make sure it finds something
		drift, err = TestCheckHRQDrift()
		Expect(err).NotTo(HaveOccurred())
		Expect(drift).Should(BeTrue())

		// And everything should be in sync again
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "11"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "11"))
	})

	It("should enqueue subtree even if the newly created HRQ has the same `spec.hard` and `status.hard`", func() {
		// Verify RQ singletons in the subtree are not created before the test.
		Eventually(getRQHard(ctx, barName, api.ResourceQuotaSingletonName)).Should(equalRL())
		Eventually(getRQHard(ctx, bazName, api.ResourceQuotaSingletonName)).Should(equalRL())
		// Create an HRQ with the same `spec.hard` and `status.hard`.
		setHRQwithSameStatus(ctx, fooHRQName, fooName, "secrets", "6", "pods", "3")
		// Verify the subtree is enqueued to update.
		Eventually(getRQHard(ctx, barName, api.ResourceQuotaSingletonName)).Should(equalRL("secrets", "6", "pods", "3"))
		Eventually(getRQHard(ctx, bazName, api.ResourceQuotaSingletonName)).Should(equalRL("secrets", "6", "pods", "3"))
	})

	It("should update usages in status correctly after moving namespace out of subtree", func() {
		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")
		setHRQ(ctx, barHRQName, barName, nil, "secrets", "100", "cpu", "50")
		setHRQ(ctx, bazHRQName, bazName, nil, "pods", "1")
		// Simulate the K8s ResourceQuota controller to update usages.
		updateRQUsage(ctx, fooName, api.ResourceQuotaSingletonName, "secrets", "0", "pods", "0")
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "0", "cpu", "0", "pods", "0")
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "secrets", "0", "pods", "0")

		// Increase pods count from 0 to 1 in baz and verify that the usage is
		// increased in foo's HRQ but not bar's (not an ancestor of baz)
		updateRQUsage(ctx, bazName, api.ResourceQuotaSingletonName, "pods", "1")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "0", "pods", "1"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "0", "cpu", "0"))
		Eventually(getHRQUsed(ctx, bazName, bazHRQName)).Should(equalRL("pods", "1"))

		// Make baz a full namespace by changing its parent to nil
		bazHier := GetHierarchy(ctx, bazName)
		bazHier.Spec.Parent = ""
		UpdateHierarchy(ctx, bazHier)

		Eventually(HasChild(ctx, fooName, bazName)).ShouldNot(Equal(true))

		// Ensure pods usage is decreased on foo after the change in hierarchy
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "0", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "0", "cpu", "0"))
		Eventually(getHRQUsed(ctx, bazName, bazHRQName)).Should(equalRL("pods", "1"))
	})

	It("should update usages in status correctly after moving full namespace with limits into hierarchy", func() {
		// Make bar a full namespace by changing its parent to nil
		barHier := GetHierarchy(ctx, barName)
		barHier.Spec.Parent = ""
		UpdateHierarchy(ctx, barHier)

		Eventually(HasChild(ctx, fooName, barName)).ShouldNot(Equal(true))

		setHRQ(ctx, fooHRQName, fooName, nil, "secrets", "6", "pods", "3")
		setHRQ(ctx, barHRQName, barName, nil, "secrets", "100", "cpu", "50")

		// Simulate the K8s ResourceQuota controller to update usages.
		updateRQUsage(ctx, fooName, api.ResourceQuotaSingletonName, "secrets", "0", "pods", "0")
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "0", "cpu", "0", "pods", "0")

		// Increase secrets count from 0 to 1 in bar and verify that the usage is
		// increased in bar's HRQ but not foo's (not an ancestor of baz)
		updateRQUsage(ctx, barName, api.ResourceQuotaSingletonName, "secrets", "1")
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "0", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "1", "cpu", "0"))

		// Make bar a full namespace by changing its parent to nil
		barHier = GetHierarchy(ctx, barName)
		barHier.Spec.Parent = fooName
		UpdateHierarchy(ctx, barHier)

		Eventually(HasChild(ctx, fooName, barName)).Should(Equal(true))

		// Ensure secrets usage is decreased on foo after the change in hierarchy
		Eventually(getHRQUsed(ctx, fooName, fooHRQName)).Should(equalRL("secrets", "1", "pods", "0"))
		Eventually(getHRQUsed(ctx, barName, barHRQName)).Should(equalRL("secrets", "1", "cpu", "0"))
	})
})

func forestOverrideSubtreeUsages(ns string, args ...string) {
	TestForest.Lock()
	defer TestForest.Unlock()
	TestForest.Get(ns).TestOnlySetSubtreeUsage(argsToResourceList(0, args...), rqName)
}

func getHRQStatus(ctx context.Context, ns, nm string) func() v1.ResourceList {
	return func() v1.ResourceList {
		nsn := types.NamespacedName{Namespace: ns, Name: nm}
		inst := &api.HierarchicalResourceQuota{}
		if err := K8sClient.Get(ctx, nsn, inst); err != nil {
			return nil
		}
		return inst.Status.Hard
	}
}

func getHRQUsed(ctx context.Context, ns, nm string) func() v1.ResourceList {
	return func() v1.ResourceList {
		nsn := types.NamespacedName{Namespace: ns, Name: nm}
		inst := &api.HierarchicalResourceQuota{}
		if err := K8sClient.Get(ctx, nsn, inst); err != nil {
			return nil
		}
		return inst.Status.Used
	}
}

func getRQHard(ctx context.Context, ns, name string) func() v1.ResourceList {
	return func() v1.ResourceList {
		nsn := types.NamespacedName{Namespace: ns, Name: name}
		inst := &v1.ResourceQuota{}
		if err := K8sClient.Get(ctx, nsn, inst); err != nil {
			return nil
		}
		return inst.Spec.Hard
	}
}

func getRQScoep(ctx context.Context, ns, name string) func() *v1.ScopeSelector {
	return func() *v1.ScopeSelector {
		nsn := types.NamespacedName{Namespace: ns, Name: name}
		inst := &v1.ResourceQuota{}
		if err := K8sClient.Get(ctx, nsn, inst); err != nil {
			return nil
		}
		return inst.Spec.ScopeSelector
	}
}

func deleteHierarchicalResourceQuota(ctx context.Context, ns, nm string) {
	inst := &api.HierarchicalResourceQuota{}
	inst.SetNamespace(ns)
	inst.SetName(nm)
	EventuallyWithOffset(1, func() error {
		return K8sClient.Delete(ctx, inst)
	}).Should(Succeed())
}

// setHRQwithSameStatus creates or replaces an existing HRQ with the given
// resource limits. Any existing spec and status are replaced by this function.
func setHRQwithSameStatus(ctx context.Context, nm, ns string, args ...string) {
	nsn := types.NamespacedName{Namespace: ns, Name: nm}
	hrq := &api.HierarchicalResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nm,
			Namespace: ns,
		},
	}
	EventuallyWithOffset(1, func() error {
		err := K8sClient.Get(ctx, nsn, hrq)
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}).Should(Succeed(), "While checking if HRQ %s/%s already exists", ns, nm)

	hrq.Spec.Hard = argsToResourceList(1, args...)
	hrq.Status.Hard = argsToResourceList(1, args...)

	EventuallyWithOffset(1, func() error {
		if hrq.CreationTimestamp.IsZero() {
			err := K8sClient.Create(ctx, hrq)
			if err == nil {
				createdHRQs = append(createdHRQs, hrq)
			}
			return err
		} else {
			return K8sClient.Update(ctx, hrq)
		}
	}).Should(Succeed(), "While updating HRQ; %+v", hrq)
}
