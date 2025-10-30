package namespaces

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/config"
)

const (
	// MutatorServingPath is where the mutator will run. Must be kept in
	// sync with the kubebuilder markers below.
	MutatorServingPath = "/mutate-namespace"
)

// Note: the mutating webhook FAILS OPEN. This means that if the webhook goes
// down, all further changes are allowed. (An empty line has to be kept below
// the kubebuilder marker for the controller-gen to generate manifests.)
//
// +kubebuilder:webhook:admissionReviewVersions=v1,path=/mutate-namespace,mutating=true,failurePolicy=ignore,groups="",resources=namespaces,sideEffects=None,verbs=create;update,versions=v1,name=namespacelabel.hnc.x-k8s.io

type Mutator struct {
	Log logr.Logger
}

func NewMutator() *Mutator {
	return &Mutator{
		Log: ctrl.Log.WithName("namespace").WithName("mutate"),
	}
}

func (m *Mutator) Default(ctx context.Context, obj runtime.Object) error {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil
	}

	log := m.Log.WithValues("namespace", ns.Name)
	m.mutateNamespace(log, ns)
	return nil
}

// mutateNamespace implements the non-boilerplate logic of this mutator, allowing it to
// be more easily unit tested (ie without constructing a full admission.Request).
// Currently, we only add `included-namespace` label to non-excluded namespaces
// if the label is missing.
func (m *Mutator) mutateNamespace(log logr.Logger, ns *corev1.Namespace) {
	if !config.IsManagedNamespace(ns.Name) {
		return
	}

	// Add label if the namespace doesn't have it.
	if _, hasLabel := ns.Labels[api.LabelIncludedNamespace]; !hasLabel {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		log.Info("Managed namespace is missing included-namespace label; adding")
		ns.Labels[api.LabelIncludedNamespace] = "true"
	}
}
