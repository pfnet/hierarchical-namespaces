package hrq

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	k8sadm "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
	"sigs.k8s.io/hierarchical-namespaces/internal/forest"
	"sigs.k8s.io/hierarchical-namespaces/internal/logutils"
)

const (
	// ResourceQuotasStatusServingPath is where the validator will run. Must be kept in sync with the
	// kubebuilder markers below.
	ResourceQuotasStatusServingPath          = "/validate-hnc-x-k8s-io-v1alpha2-resourcequotasstatus"
	ResourceQuotaAdmissionControllerUsername = "system:apiserver"
)

// TODO: Change to fail closed after we add specific labels on the excluded namespaces so there
// won't be a deadlock of our pod not responding while it is also not allowed to be created in the
// "hnc-system" namespace.  Note: the validating webhook FAILS OPEN. This means that if the webhook
// goes down, all further changes are allowed.
//
// +<remove when ready>kubebuilder:webhook:admissionReviewVersions=v1;v1beta1,path=/validate-hnc-x-k8s-io-v1alpha2-resourcequotasstatus,mutating=false,failurePolicy=ignore,groups="",resources=resourcequotas/status,sideEffects=None,verbs=update,versions=v1,name=resourcesquotasstatus.hnc.x-k8s.io

type ResourceQuotaStatus struct {
	Log    logr.Logger
	Forest *forest.Forest
	client client.Client
}

func NewResourceQuotaStatus(forest *forest.Forest, client client.Client) *ResourceQuotaStatus {
	return &ResourceQuotaStatus{
		Log:    ctrl.Log.WithName("validators").WithName("ResourceQuota"),
		Forest: forest,
		client: client,
	}
}

func (r *ResourceQuotaStatus) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// ResourceQuota creation doesn't need validation for our purposes
	return nil, nil
}

func (r *ResourceQuotaStatus) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	log := logutils.WithRID(r.Log).WithValues("nm", req.Name, "nnm", req.Namespace)

	rq, ok := newObj.(*v1.ResourceQuota)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("expected a ResourceQuota but got a %T", newObj))
	}

	if err := r.validateResourceQuotaStatus(ctx, log, &req.UserInfo, rq); err != nil {
		log.Info("Denied", "reason", err)
		return nil, err
	}

	log.V(1).Info("Allowed")
	return nil, nil
}

func (r *ResourceQuotaStatus) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// ResourceQuota deletion doesn't need validation for our purposes
	return nil, nil
}

func (r *ResourceQuotaStatus) validateResourceQuotaStatus(_ context.Context, log logr.Logger, userInfo *authnv1.UserInfo, inst *v1.ResourceQuota) error {
	// We only intercept and validate the HRQ-singleton RQ status changes from the
	// K8s Resource Quota Admission Controller. Once the admission controller
	// allows a request, it will issue a request to update the RQ status. We can
	// control whether a resource should (or should not) be consumed by allowing
	// (or denying) status update requests from the admission controller.
	//
	// Thus we will allow any non-HRQ-singleton RQ updates first.
	if !strings.Contains(inst.Name, api.ResourceQuotaSingletonName) {
		log.V(1).Info("non-HRQ-singleton RQ status change ignored")
		return nil
	}

	// Then for HRQ-singleton RQ status updates, we will allow any request
	// attempted by other than K8s Resource Quota Admission Controller.
	if !isResourceQuotaAdmissionControllerServiceAccount(userInfo) {
		log.V(1).Info("Request is not from Kubernetes Resource Quota Admission Controller; Allowed")
		return nil
	}

	return r.validateHRQResourceQuotaStatus(inst)
}

func (r *ResourceQuotaStatus) validateHRQResourceQuotaStatus(inst *v1.ResourceQuota) error {
	r.Forest.Lock()
	defer r.Forest.Unlock()

	ns := r.Forest.Get(inst.Namespace)
	if err := ns.TryUseResources(inst.Status.Used, inst.GetName()); err != nil {
		return apierrors.NewForbidden(v1.Resource("resourcequotas"), inst.Name, err)
	}

	return nil
}

func isResourceQuotaAdmissionControllerServiceAccount(user *authnv1.UserInfo) bool {
	// Treat nil user same as admission controller SA so that unit tests do not need to
	// specify admission controller SA.
	return user == nil || user.Username == ResourceQuotaAdmissionControllerUsername
}

// Helper functions for admission responses - these are shared between hrqvalidator.go and rqstatusvalidator.go

// allow is a replacement for controller-runtime's admission.Allowed() that allows you to set the
// message (human-readable) as opposed to the reason (machine-readable).
func allow(msg string) admission.Response {
	return admission.Response{AdmissionResponse: k8sadm.AdmissionResponse{
		Allowed: true,
		Result: &metav1.Status{
			Code:    0,
			Message: msg,
		},
	}}
}

// deny is a replacement for controller-runtime's admission.Denied() that allows you to set _both_ a
// human-readable message _and_ a machine-readable reason, and also sets the code correctly instead
// of hardcoding it to 403 Forbidden.
func deny(reason metav1.StatusReason, msg string) admission.Response {
	return admission.Response{AdmissionResponse: k8sadm.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Code:    codeFromReason(reason),
			Message: msg,
			Reason:  reason,
		},
	}}
}

// denyInvalidField sets "invalid" as the reason and allows you to set which
// field is invalid and a human-readable message.
//
// Note: this code piece is first copied from OSS HNC, so that the denial
// message can show up for invalid requests. Then a "field" field is added so
// that the denial message is shown properly with the culprit field instead of
// double colons (see https://github.com/kubernetes-sigs/multi-tenancy/issues/1218).
func denyInvalidField(field, msg string) admission.Response {
	// We need to set the custom message in both Details and Message fields.
	//
	// When manipulating the HRQ object via kubectl directly, kubectl
	// ignores the Message field and displays the Details field if an error is
	// StatusReasonInvalid (see implementation here: https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/util/helpers.go#L145-L160).
	//
	// When manipulating the HRQ object via the hns kubectl plugin,
	// if an error is StatusReasonInvalid, only the Message field will be displayed. This is because
	// the Error method (https://github.com/kubernetes/client-go/blob/cb664d40f84c27bee45c193e4acb0fcd549b0305/rest/request.go#L1273)
	// calls FromObject (https://github.com/kubernetes/apimachinery/blob/7e441e0f246a2db6cf1855e4110892d1623a80cf/pkg/api/errors/errors.go#L100),
	// which generates a StatusError (https://github.com/kubernetes/apimachinery/blob/7e441e0f246a2db6cf1855e4110892d1623a80cf/pkg/api/errors/errors.go#L35) object.
	// *StatusError implements the Error interface using only the Message
	// field (https://github.com/kubernetes/apimachinery/blob/7e441e0f246a2db6cf1855e4110892d1623a80cf/pkg/api/errors/errors.go#L49)).
	// Therefore, when displaying the error, only the Message field will be available.
	resp := deny(metav1.StatusReasonInvalid, msg)
	resp.Result.Details = &metav1.StatusDetails{
		Causes: []metav1.StatusCause{
			{
				Message: msg,
				Field:   field,
			},
		},
	}
	return resp
}

// codeFromReason implements the needed subset of
// https://godoc.org/k8s.io/apimachinery/pkg/apis/meta/v1#StatusReason
func codeFromReason(reason metav1.StatusReason) int32 {
	switch reason {
	case metav1.StatusReasonUnknown:
		return 500
	case metav1.StatusReasonUnauthorized:
		return 401
	case metav1.StatusReasonForbidden:
		return 403
	case metav1.StatusReasonConflict:
		return 409
	case metav1.StatusReasonBadRequest:
		return 400
	case metav1.StatusReasonInvalid:
		return 422
	case metav1.StatusReasonInternalError:
		return 500
	default:
		return 500
	}
}
