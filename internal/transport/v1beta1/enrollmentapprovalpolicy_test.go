package transportv1beta1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	convert "github.com/flightctl/flightctl/internal/api/convert/v1beta1"
	"github.com/flightctl/flightctl/internal/auth"
	"github.com/flightctl/flightctl/internal/domain"
	policyservice "github.com/flightctl/flightctl/internal/service/enrollmentapprovalpolicy"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestApprovalPolicyPatchPreservesInt64Limit(t *testing.T) {
	controller := gomock.NewController(t)
	permissions := auth.NewMockAuthZMiddleware(controller)
	permissions.EXPECT().CheckPermission(gomock.Any(), "enrollmentapprovalpolicies/approvallimit", "update").Return(true, nil)
	service := policyservice.NewMockService(controller)
	service.EXPECT().PatchEnrollmentApprovalPolicy(gomock.Any(), gomock.Any(), "west", gomock.Any()).Do(func(_ any, _ any, _ any, patch domain.PatchRequest) {
		require.Equal(t, json.Number("9223372036854775807"), patch[0].Value)
	}).Return(nil, domain.StatusOK())
	handler := &TransportHandler{converter: convert.NewConverter(), authZ: permissions, enrollmentapprovalpolicy: service}
	response := httptest.NewRecorder()
	handler.PatchEnrollmentApprovalPolicy(response, httptest.NewRequest(http.MethodPatch, "/enrollmentapprovalpolicies/west", strings.NewReader(`[{"op":"add","path":"/spec/approvalLimit","value":9223372036854775807}]`)), "west")
	require.Equal(t, http.StatusOK, response.Code)
}

func TestApprovalPolicyRejectsUntrustedStatus(t *testing.T) {
	for _, body := range []string{`{"status":null}`, `{"status":{"enrollmentCount":0}}`, `{"spec":{"unknown":true}}`, `{} {}`} {
		t.Run(body, func(t *testing.T) {
			handler := &TransportHandler{converter: convert.NewConverter(), enrollmentapprovalpolicy: policyservice.NewMockService(gomock.NewController(t))}
			response := httptest.NewRecorder()
			handler.CreateEnrollmentApprovalPolicy(response, httptest.NewRequest(http.MethodPost, "/enrollmentapprovalpolicies", strings.NewReader(body)))
			require.Equal(t, http.StatusBadRequest, response.Code)
		})
	}
}

func TestApprovalPolicyPatchRejectsTrailingDocument(t *testing.T) {
	handler := &TransportHandler{converter: convert.NewConverter(), enrollmentapprovalpolicy: policyservice.NewMockService(gomock.NewController(t))}
	response := httptest.NewRecorder()
	handler.PatchEnrollmentApprovalPolicy(response, httptest.NewRequest(http.MethodPatch, "/enrollmentapprovalpolicies/west", strings.NewReader(`[] {}`)), "west")
	require.Equal(t, http.StatusBadRequest, response.Code)
}
