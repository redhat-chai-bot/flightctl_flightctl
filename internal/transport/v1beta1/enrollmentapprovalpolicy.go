package transportv1beta1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	apiv1beta1 "github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/transport"
)

func policyFailure(code int32, reason, message string) domain.Status {
	return domain.NewFailureStatus(code, reason, message)
}

func (handler *TransportHandler) policyAvailable(w http.ResponseWriter) bool {
	if handler.enrollmentapprovalpolicy == nil {
		handler.SetResponse(w, nil, policyFailure(503, "PolicyValidationUnavailable", "policy service is unavailable"))
		return false
	}
	return true
}

func decodePolicy(body io.Reader, target any) error {
	var raw json.RawMessage
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("exactly one JSON document is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, exists := fields["status"]; exists {
		return fmt.Errorf("status is server-owned")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	return strict.Decode(target)
}

func (handler *TransportHandler) quotaAllowed(w http.ResponseWriter, request *http.Request) bool {
	if handler.authZ != nil {
		allowed, err := handler.authZ.CheckPermission(request.Context(), "enrollmentapprovalpolicies/approvallimit", "update")
		if err == nil && allowed {
			return true
		}
	}
	handler.SetResponse(w, nil, policyFailure(403, "Forbidden", "approval limit changes require quota permission"))
	return false
}

func (handler *TransportHandler) CreateEnrollmentApprovalPolicy(w http.ResponseWriter, request *http.Request) {
	if !handler.policyAvailable(w) {
		return
	}
	var policy apiv1beta1.EnrollmentApprovalPolicy
	if err := decodePolicy(request.Body, &policy); err != nil {
		handler.SetResponse(w, nil, policyFailure(400, "ProtectedField", err.Error()))
		return
	}
	if policy.Spec.ApprovalLimit != nil && !handler.quotaAllowed(w, request) {
		return
	}
	result, status := handler.enrollmentapprovalpolicy.CreateEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), policy)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) ListEnrollmentApprovalPolicies(w http.ResponseWriter, request *http.Request, params apiv1beta1.ListEnrollmentApprovalPoliciesParams) {
	if !handler.policyAvailable(w) {
		return
	}
	result, status := handler.enrollmentapprovalpolicy.ListEnrollmentApprovalPolicies(request.Context(), transport.OrgIDFromContext(request.Context()), params)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) GetEnrollmentApprovalPolicy(w http.ResponseWriter, request *http.Request, name string) {
	if !handler.policyAvailable(w) {
		return
	}
	result, status := handler.enrollmentapprovalpolicy.GetEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), name)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) GetEnrollmentApprovalPolicyStatus(w http.ResponseWriter, request *http.Request, name string) {
	if !handler.policyAvailable(w) {
		return
	}
	result, status := handler.enrollmentapprovalpolicy.GetEnrollmentApprovalPolicyStatus(request.Context(), transport.OrgIDFromContext(request.Context()), name)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) ReplaceEnrollmentApprovalPolicy(w http.ResponseWriter, request *http.Request, name string) {
	if !handler.policyAvailable(w) {
		return
	}
	var policy apiv1beta1.EnrollmentApprovalPolicy
	if err := decodePolicy(request.Body, &policy); err != nil {
		handler.SetResponse(w, nil, policyFailure(400, "ProtectedField", err.Error()))
		return
	}
	current, status := handler.enrollmentapprovalpolicy.GetEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), name)
	if current == nil {
		handler.SetResponse(w, nil, status)
		return
	}
	if policy.Metadata.ResourceVersion == nil || current.Metadata.ResourceVersion == nil || *policy.Metadata.ResourceVersion != *current.Metadata.ResourceVersion {
		handler.SetResponse(w, nil, policyFailure(409, "ResourceVersionConflict", "current resourceVersion is required"))
		return
	}
	if !reflect.DeepEqual(current.Spec.ApprovalLimit, policy.Spec.ApprovalLimit) && !handler.quotaAllowed(w, request) {
		return
	}
	result, status := handler.enrollmentapprovalpolicy.ReplaceEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), name, policy)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) PatchEnrollmentApprovalPolicy(w http.ResponseWriter, request *http.Request, name string) {
	if !handler.policyAvailable(w) {
		return
	}
	var patch apiv1beta1.PatchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		handler.SetResponse(w, nil, policyFailure(400, "InvalidPolicy", err.Error()))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		handler.SetResponse(w, nil, policyFailure(400, "InvalidPolicy", "exactly one JSON document is required"))
		return
	}
	for _, operation := range patch {
		if operation.Path == "/spec" || operation.Path == "/spec/approvalLimit" || strings.HasPrefix(operation.Path, "/spec/approvalLimit/") {
			if !handler.quotaAllowed(w, request) {
				return
			}
		}
	}
	result, status := handler.enrollmentapprovalpolicy.PatchEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), name, patch)
	handler.SetResponse(w, result, status)
}

func (handler *TransportHandler) DeleteEnrollmentApprovalPolicy(w http.ResponseWriter, request *http.Request, name string) {
	if !handler.policyAvailable(w) {
		return
	}
	status := handler.enrollmentapprovalpolicy.DeleteEnrollmentApprovalPolicy(request.Context(), transport.OrgIDFromContext(request.Context()), name)
	handler.SetResponse(w, nil, status)
}
