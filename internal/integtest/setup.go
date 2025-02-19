/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integtest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //lint:ignore ST1001 Ignoring this for now
	. "github.com/onsi/gomega"    //lint:ignore ST1001 Ignoring this for now
	apiadmissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/hierarchical-namespaces/internal/objects"
	"sigs.k8s.io/hierarchical-namespaces/internal/webhooks"

	// +kubebuilder:scaffold:imports

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/config"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/setup"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	K8sClient          client.Client
	testEnv            *envtest.Environment
	k8sManagerCancelFn context.CancelFunc
	TestForest         *forest.Forest
)

func HNCRun(t *testing.T, title string) {
	RunSpecs(t, title)
}

// All tests in the reconcilers_test package are in one suite. As a result, they
// share the same test environment (e.g., same api server).
func HNCBeforeSuite() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	RegisterFailHandler(Fail)

	// Prow machines appear to be very overloaded. While 2s seems to work just fine on a workstation
	// (1s is usually not enough), we've seen some errors on Prow that can only really be attributed
	// to needing more time. So let's set this to 4s for now.
	// - aludwin, Oct 2020
	SetDefaultEventuallyTimeout(time.Second * 4)

	By("configuring test environment")
	sideEffectClassNone := apiadmissionregistrationv1.SideEffectClassNone
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*apiadmissionregistrationv1.ValidatingWebhookConfiguration{{
				ObjectMeta: metav1.ObjectMeta{
					Name: webhooks.ValidatingWebhookConfigurationName,
				},
				Webhooks: []apiadmissionregistrationv1.ValidatingWebhook{{
					Name:                    webhooks.ObjectsWebhookName,
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             &sideEffectClassNone,
					ClientConfig: apiadmissionregistrationv1.WebhookClientConfig{
						Service: &apiadmissionregistrationv1.ServiceReference{
							Namespace: "system",
							Name:      "webhook-service",
							Path:      pointer.String(objects.ServingPath),
						},
					},
				}},
			}},
		},
	}

	By("starting test environment")
	time.Sleep(10 * time.Second)
	cfg, err := testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	By("updating scheme")
	err = api.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = apiextensions.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	// CF: https://github.com/microsoft/azure-databricks-operator/blob/0f722a710fea06b86ecdccd9455336ca712bf775/controllers/suite_test.go

	By("creating manager")
	webhookInstallOptions := &testEnv.WebhookInstallOptions
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		NewClient:          config.NewClient(false),
		MetricsBindAddress: "0", // disable metrics serving since 'go test' runs multiple suites in parallel processes
		Scheme:             scheme.Scheme,
		Host:               webhookInstallOptions.LocalServingHost,
		Port:               webhookInstallOptions.LocalServingPort,
		CertDir:            webhookInstallOptions.LocalServingCertDir,
	})
	Expect(err).ToNot(HaveOccurred())

	// Register a dummy webhook since the test control plane is to test reconcilers
	k8sManager.GetWebhookServer().Register(objects.ServingPath, &webhook.Admission{Handler: &allowAllHandler{}})

	By("creating reconcilers")
	opts := setup.Options{
		MaxReconciles: 100,
		UseFakeClient: true,
		HNCCfgRefresh: 1 * time.Second, // so we don't have to wait as long
		HRQ:           true,
	}
	TestForest = forest.NewForest()
	err = setup.CreateReconcilers(k8sManager, TestForest, opts)
	Expect(err).ToNot(HaveOccurred())

	By("Creating clients")
	K8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(K8sClient).ToNot(BeNil())

	go func() {
		var ctx context.Context
		ctx, k8sManagerCancelFn = context.WithCancel(ctrl.SetupSignalHandler())
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()
}

type allowAllHandler struct{}

func (a allowAllHandler) Handle(_ context.Context, _ admission.Request) admission.Response {
	return webhooks.Allow("All requests are allowed by allowAllHandler")
}

func HNCAfterSuite() {
	if k8sManagerCancelFn != nil {
		k8sManagerCancelFn()
	}
	k8sManagerCancelFn = nil
	By("tearing down the test environment")
	Expect(testEnv).ToNot(BeNil())
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
}

func TestCheckHRQDrift() (bool, error) {
	return setup.TestOnlyCheckHRQDrift(TestForest)
}
