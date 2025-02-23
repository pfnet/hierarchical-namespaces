package hrq

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/hrq/utils"
)

func TestRQStatusChange(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		consume   []string
		fail      bool
	}{
		{name: "allow 1 more configmap in parent", namespace: "a", consume: []string{"configmaps", "1"}},
		{name: "allow 1 configmap in child", namespace: "b", consume: []string{"configmaps", "1"}},
		{name: "deny 2 more configmaps in parent (violating a-hrq)", namespace: "a", consume: []string{"configmaps", "2"}, fail: true},
		{name: "deny 2 configmaps in child (violating a-hrq)", namespace: "b", consume: []string{"configmaps", "2"}, fail: true},
		{name: "allow 1 more secret in child", namespace: "b", consume: []string{"secrets", "1"}},
		{name: "allow 1 secret in parent", namespace: "a", consume: []string{"secrets", "1"}},
		{name: "deny 2 more secrets in child (violating a-hrq)", namespace: "b", consume: []string{"secrets", "2"}, fail: true},
		{name: "deny 2 secrets in parent (violating a-hrq)", namespace: "a", consume: []string{"secrets", "2"}, fail: true},
		{name: "allow any other resources", namespace: "a", consume: []string{"pods", "100"}},
		{name: "allow 1 more configmap in parent and other resources", namespace: "a", consume: []string{"configmaps", "1", "pods", "100"}},
		{name: "deny 2 more configmaps in parent (violating a-hrq) together with other resources", namespace: "a", consume: []string{"configmaps", "2", "pods", "100"}, fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a forest with namespace 'a' as the root and 'b' as a child.
			f := forest.NewForest()
			nsA := f.Get("a")
			nsB := f.Get("b")
			err := nsB.SetParent(nsA)
			if err != nil {
				t.Fatalf("failed to set parent: %v", err)
			}

			// Add an HRQ with limits of 2 secrets and 2 configmaps in 'a'.
			nsA.UpdateLimits("a-hrq", api.ResourceQuotaSingletonName, argsToResourceList("configmaps", "2", "secrets", "2"))
			// Add a looser HRQ with limits of 10 secrets and 10 configmaps in 'b'.
			nsB.UpdateLimits("b-hrq", api.ResourceQuotaSingletonName, argsToResourceList("configmaps", "10", "secrets", "10"))

			// Consume 1 configmap in 'a' and 1 secret in 'b'.
			err = nsA.UseResources(api.ResourceQuotaSingletonName, argsToResourceList("configmaps", "1"))
			if err != nil {
				t.Fatalf("failed to use resources for %s: %v", nsA.Name(), err)
			}
			err = nsB.UseResources(api.ResourceQuotaSingletonName, argsToResourceList("secrets", "1"))
			if err != nil {
				t.Fatalf("failed to use resources for %s: %v", nsB.Name(), err)
			}
			rqs := &ResourceQuotaStatus{Forest: f}

			// Construct the requested instance from the delta usages specified in the
			// test case and what's in forest.
			rqInst := &v1.ResourceQuota{}
			rqInst.Name = api.ResourceQuotaSingletonName
			rqInst.Namespace = tc.namespace
			delta := argsToResourceList(tc.consume...)
			usage, _ := f.Get(tc.namespace).GetLocalUsages(api.ResourceQuotaSingletonName)
			rqInst.Status.Used = utils.Add(delta, usage)

			got := rqs.handle(rqInst)
			if got.AdmissionResponse.Allowed == tc.fail {
				t.Errorf("unexpected admission response")
			}
		})
	}
}

// argsToResourceList provides a convenient way to specify a resource list by
// interpreting even-numbered args as resource names (e.g. "secrets") and
// odd-valued args as quantities (e.g. "5", "1Gb", etc).
func argsToResourceList(args ...string) v1.ResourceList {
	list := map[v1.ResourceName]resource.Quantity{}
	for i := 0; i < len(args); i += 2 {
		list[v1.ResourceName(args[i])] = resource.MustParse(args[i+1])
	}
	return list
}
