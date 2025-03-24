package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/yaml"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/hierarchical-namespaces/pkg/testutils"
)

var _ = Describe("Scoped Hierarchical Resource Quota", Label("pfnet"), func() {
	var (
		parentNs, childNs, priorityName     string
		cleanupNs, cleanupPriority     func()
		scopeSelector corev1.ScopeSelector
		normalHRQ api.HierarchicalResourceQuota
	)

	BeforeEach(func() {
		parentNs, childNs, cleanupNs = setUpParentAndChild()
		scopeSelector, priorityName, cleanupPriority = genPriorityScopeSelector()

		// For checking not-scoped HRQ with scoped HRQ.
		normalHRQ = setScopedHRQ("normal-hrq", parentNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")}, nil)
	})

	AfterEach(func() {
		var podCount int
		for _, ns := range []string{parentNs, childNs} {
			out, err := RunCommand("kubectl get --no-headers pods -n " + ns)
			Expect(err).NotTo(HaveOccurred())
			if strings.Contains(out, "No resources found") {
				continue
			}

			pods := strings.Count(out, "\n")
			Expect(err).NotTo(HaveOccurred())
			podCount += pods
		}

		FieldShouldContainWithTimeout("hrq", parentNs, normalHRQ.Name, ".status.used", "pods:"+strconv.Itoa(podCount), 300)

		// For debugging after failed tests
		if !CurrentSpecReport().Failed() {
			cleanupPriority()
			cleanupNs()
		}
	})

	It("should create RQs with correct limits in the descendants (including itself) for Scoped HRQs", func() {
		hrqA := setScopedHRQ("a-hrq", parentNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")}, &scopeSelector)
		hrqB := setScopedHRQ("b-hrq", childNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")}, &scopeSelector)

		rqAName := api.ResourceQuotaSingletonName + "-" + hrqA.Name
		rqBName := api.ResourceQuotaSingletonName + "-" + hrqB.Name

		FieldShouldContain("resourcequota", parentNs, rqAName, ".spec.hard", "pods:3")
		FieldShouldContain("resourcequota", childNs, rqBName, ".spec.hard", "pods:2")

		expect := selectorStr(priorityName)
		FieldShouldContain("resourcequota", parentNs, rqAName, ".spec.scopeSelector", expect)
		FieldShouldContain("resourcequota", childNs, rqBName, ".spec.scopeSelector", expect)
	})

	It("should remove obsolete (empty) RQ if there's no longer a Scoped HRQ in the ancestor", func() {
		hrq := setScopedHRQ("a-hrq", parentNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")}, &scopeSelector)
		rqName := api.ResourceQuotaSingletonName + "-" + hrq.Name

		MustRun("kubectl delete hrq -n", parentNs, hrq.Name)

		RunShouldNotContain(rqName, propagationTime, "kubectl get resourcequota -n" , parentNs)
		RunShouldNotContain(rqName, propagationTime, "kubectl get resourcequota -n" , childNs)
	})

	It("should update the .status.used field of the HRQ when pods are created", func() {
		hrq := setScopedHRQ("status-updated", parentNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")}, &scopeSelector)

		for i := 0; i < 2; i++ {
			_, err := mustCreatePodWithPrioirty(fmt.Sprint(i), childNs, priorityName)
			Expect(err).NotTo(HaveOccurred())
			FieldShouldContain("hrq", parentNs, hrq.Name, ".status.used", "pods:"+fmt.Sprint(i+1))
		}

		_, err := mustCreatePod("normal", childNs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should reject creating a pod if the HRQ is exceeded", func() {
		hrq := setScopedHRQ("ok-ng", parentNs, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1")}, &scopeSelector)

		_, err := mustCreatePodWithPrioirty("ok", childNs, priorityName)
		Expect(err).NotTo(HaveOccurred())
		FieldShouldContain("hrq", parentNs, hrq.Name, ".status.used", "pods:1")

		_, err = mustCreatePodWithPrioirty("ng", childNs, priorityName)
		Expect(err).ShouldNot(Succeed())

		_, err = mustCreatePod("normal-pod", childNs)
		Expect(err).NotTo(HaveOccurred())

		FieldShouldContain("hrq", parentNs, hrq.Name, ".status.used", "pods:1")
	})
})

func mustCreatePod(prefix, nsnm string) (corev1.Pod, error) {
	return mustCreatePodWithPrioirty(prefix, nsnm, "")
}

func mustCreatePodWithPrioirty(prefix, nsnm, priority string) (corev1.Pod, error) {
	name := prefix + "-" +  uuid.New().String()
	spec := corev1.PodSpec{
		PriorityClassName: priority,
		Containers: []corev1.Container{
			{
				Name:  "test",
				Image: "nginx",
			},
		},
	}
	if priority != "" {
		spec.PriorityClassName = priority
	}

	pod := corev1.Pod {
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsnm,
		},
		Spec: spec,
	}

	manifest, err := yaml.Marshal(pod)
	Expect(err).NotTo(HaveOccurred())

	fn := writeTempFile(string(manifest))
	GinkgoT().Log("Wrote " + fn + ":\n" + string(manifest))
	defer removeFile(fn)

	return pod, TryRun("kubectl apply -f " + fn)
}

func setScopedHRQ(nm, nsnm string, resourceList corev1.ResourceList, scopeSelector *corev1.ScopeSelector) api.HierarchicalResourceQuota {
	hrq := api.HierarchicalResourceQuota{
		TypeMeta: metav1.TypeMeta{
			Kind:       "HierarchicalResourceQuota",
			APIVersion: "hnc.x-k8s.io/v1alpha2",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nm,
			Namespace: nsnm,
		},
		Spec: api.HierarchicalResourceQuotaSpec{
			Hard: resourceList,
			ScopeSelector: scopeSelector,
		},
	}
	manifest, err := yaml.Marshal(hrq)
	Expect(err).NotTo(HaveOccurred())

	MustApplyYAML(string(manifest))
	RunShouldContain(nm, propagationTime, "kubectl get hrq -n", nsnm, nm)

	return hrq
}

func setUpParentAndChild() (string, string, func()) {
	parentNs := createNS("hrq-test-parent-")
	childNs := createSubNS(parentNs)
	cleanup := func() {
		MustRunWithTimeout(cleanupTimeout, "kubectl annotate ns", parentNs, "hnc.x-k8s.io/subnamespace-of-")
		MustRunWithTimeout(cleanupTimeout, "kubectl annotate ns", childNs, "hnc.x-k8s.io/subnamespace-of-")
		var wg sync.WaitGroup
		for _, ns := range []string{parentNs, childNs} {
			wg.Add(1)
			go func(ns string) {
				MustRun("kubectl delete ns", ns)
				wg.Done()
			}(ns)
		}
		wg.Wait()
	}

	RunShouldContain(childNs, propagationTime, "kubectl get ns")
	return parentNs, childNs, cleanup
}

func createNS(prefix string) string {
	nsName := prefix + uuid.New().String()
	MustRun("kubectl create ns", nsName)
	return nsName
}

func createSubNS(parent string) string {
	nsName := "hrq-test-child-" + uuid.New().String()
	MustRun("kubectl hns create", nsName, "-n", parent)
	return nsName
}

func genPriorityScopeSelector() (corev1.ScopeSelector, string, func()) {
	priority := uuid.New().String()
	err := TryRun("kubectl create priorityclass", priority, "--value=100")
	Expect(err).NotTo(HaveOccurred())
	cleanup := func() {
		err := TryRun("kubectl delete priorityclass", priority)
		Expect(err).NotTo(HaveOccurred())
	}

	return corev1.ScopeSelector{
		MatchExpressions: []corev1.ScopedResourceSelectorRequirement{
			{
				Operator: corev1.ScopeSelectorOpIn,
				ScopeName: "PriorityClass",
				Values:   []string{priority},
			},
		},
	}, priority, cleanup
}

func selectorStr(priorityName string) string {
	return "map[matchExpressions:[map[operator:In scopeName:PriorityClass values:[" + priorityName + "]]]]"
}
