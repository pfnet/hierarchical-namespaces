package setup

import (
	"fmt"

	cert "github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/anchor"
	"sigs.k8s.io/hierarchical-namespaces/internal/config"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/hierarchyconfig"
	"sigs.k8s.io/hierarchical-namespaces/internal/hncconfig"
	"sigs.k8s.io/hierarchical-namespaces/internal/hrq"
	ns "sigs.k8s.io/hierarchical-namespaces/internal/namespace" // for some reason, by default this gets imported as "namespace*s*"
	"sigs.k8s.io/hierarchical-namespaces/internal/objects"
)

const (
	serviceName    = "hnc-webhook-service"
	vwhName        = "hnc-validating-webhook-configuration"
	mwhName        = "hnc-mutating-webhook-configuration"
	caName         = "hnc-ca"
	caOrganization = "hnc"
	secretName     = "hnc-webhook-server-cert"
	certDir        = "/tmp/k8s-webhook-server/serving-certs"
)

// ManageCerts creates all certs for webhooks. This function is called from main.go.
func ManageCerts(mgr ctrl.Manager, setupFinished chan struct{}, restartOnSecretRefresh bool) error {
	hncNamespace := config.GetHNCNamespace()
	// DNSName is <service name>.<hncNamespace>.svc
	dnsName := fmt.Sprintf("%s.%s.svc", serviceName, hncNamespace)

	return cert.AddRotator(mgr, &cert.CertRotator{
		SecretKey: types.NamespacedName{
			Namespace: hncNamespace,
			Name:      secretName,
		},
		CertDir:        certDir,
		CAName:         caName,
		CAOrganization: caOrganization,
		DNSName:        dnsName,
		IsReady:        setupFinished,
		Webhooks: []cert.WebhookInfo{{
			Type: cert.Validating,
			Name: vwhName,
		}, {
			Type: cert.Mutating,
			Name: mwhName,
		}},
		RestartOnSecretRefresh: restartOnSecretRefresh,
	})
}

// createWebhooks creates all mutators and validators.
func createWebhooks(mgr ctrl.Manager, f *forest.Forest, opts Options) error {
	decoder := admission.NewDecoder(mgr.GetScheme())

	// NOTE(ryotarai): The injecting mechanism is removed in https://github.com/kubernetes-sigs/controller-runtime/pull/2134
	// For now, the decoder and client are injected manually, but we might want to replace this with sigs.k8s.io/controller-runtime/pkg/builder.WebhookManagedBy

	// Create webhook for Hierarchy
	if err := builder.WebhookManagedBy(mgr).
		For(&api.HierarchyConfiguration{}).
		WithCustomPath(hierarchyconfig.ServingPath).
		WithValidator(hierarchyconfig.NewValidator(f, mgr.GetClient())).
		Complete(); err != nil {
		return fmt.Errorf("failed to create webhook for hierarchyconfig: %w", err)
	}

	// Create webhooks for managed objects
	{
		handler := &objects.Validator{
			Log:    ctrl.Log.WithName("objects").WithName("validate"),
			Forest: f,
		}
		handler.InjectDecoder(decoder)
		handler.InjectClient(mgr.GetClient())
		mgr.GetWebhookServer().Register(objects.ServingPath, &webhook.Admission{Handler: handler})
	}

	// Create webhook for the config
	{
		handler := &hncconfig.Validator{
			Log:    ctrl.Log.WithName("hncconfig").WithName("validate"),
			Forest: f,
		}
		handler.InjectDecoder(decoder)
		handler.InjectConfig(mgr.GetConfig())
		mgr.GetWebhookServer().Register(hncconfig.ServingPath, &webhook.Admission{Handler: handler})
	}

	// Create webhook for the subnamespace anchors.
	{
		handler := &anchor.Validator{
			Log:    ctrl.Log.WithName("anchor").WithName("validate"),
			Forest: f,
		}
		handler.InjectDecoder(decoder)
		mgr.GetWebhookServer().Register(anchor.ServingPath, &webhook.Admission{Handler: handler})
	}

	// Create webhook for the namespaces (core type).
	{
		handler := &ns.Validator{
			Log:    ctrl.Log.WithName("namespace").WithName("validate"),
			Forest: f,
		}
		handler.InjectDecoder(decoder)
		mgr.GetWebhookServer().Register(ns.ServingPath, &webhook.Admission{Handler: handler})
	}

	// Create mutator for namespace `included-namespace` label.
	{
		handler := &ns.Mutator{
			Log: ctrl.Log.WithName("namespace").WithName("mutate"),
		}
		handler.InjectDecoder(decoder)
		mgr.GetWebhookServer().Register(ns.MutatorServingPath, &webhook.Admission{Handler: handler})
	}

	if opts.HRQ {
		// Create webhook for ResourceQuota status.
		{
			handler := &hrq.ResourceQuotaStatus{
				Log:    ctrl.Log.WithName("validators").WithName("ResourceQuota"),
				Forest: f,
			}
			handler.InjectDecoder(decoder)
			handler.InjectClient(mgr.GetClient())
			mgr.GetWebhookServer().Register(hrq.ResourceQuotasStatusServingPath, &webhook.Admission{Handler: handler})
		}

		// Create webhook for HierarchicalResourceQuota spec.
		{
			handler := &hrq.HRQ{
				Log: ctrl.Log.WithName("validators").WithName("HierarchicalResourceQuota"),
			}
			handler.InjectDecoder(decoder)
			handler.InjectClient(mgr.GetClient())
			mgr.GetWebhookServer().Register(hrq.HRQServingPath, &webhook.Admission{Handler: handler})
		}
	}

	return nil
}
