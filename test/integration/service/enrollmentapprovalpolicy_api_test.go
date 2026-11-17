package service_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"time"

	api "github.com/flightctl/flightctl/api/core/v1beta1"
	convert "github.com/flightctl/flightctl/internal/api/convert/v1beta1"
	server "github.com/flightctl/flightctl/internal/api/server"
	"github.com/flightctl/flightctl/internal/auth"
	"github.com/flightctl/flightctl/internal/auth/authz"
	"github.com/flightctl/flightctl/internal/consts"
	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/identity"
	policyservice "github.com/flightctl/flightctl/internal/service/enrollmentapprovalpolicy"
	policystore "github.com/flightctl/flightctl/internal/store/enrollmentapprovalpolicy"
	"github.com/flightctl/flightctl/internal/store/model"
	transport "github.com/flightctl/flightctl/internal/transport/v1beta1"
	"github.com/flightctl/flightctl/internal/util"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

type policyAPIValidator struct{}

func (policyAPIValidator) ValidatePolicySet(context.Context, *domain.EnrollmentApprovalPolicy, []domain.EnrollmentApprovalPolicy) error {
	return nil
}

type policyAPIAuthorizer struct {
	auth.AuthZMiddleware
	static *authz.StaticAuthZ
}

func (authorizer policyAPIAuthorizer) CheckPermission(ctx context.Context, resource, operation string) (bool, error) {
	return authorizer.static.CheckPermission(ctx, resource, operation)
}

var _ = Describe("EnrollmentApprovalPolicyAPI", func() {
	var suite *ServiceTestSuite
	BeforeEach(func() {
		suite = &ServiceTestSuite{}
		suite.Setup()
		suite.Ctx = util.WithOrganizationID(suite.Ctx, suite.OrgID)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
		Expect(err).NotTo(HaveOccurred())
		issued := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		Expect(suite.DB.Create(&model.CertificateSigningRequest{Resource: model.Resource{OrgID: suite.OrgID, Name: "commissioning"}, Spec: model.MakeJSONField(domain.CertificateSigningRequestSpec{SignerName: suite.cfg.CA.DeviceEnrollmentSignerName}), Status: model.MakeJSONField(domain.CertificateSigningRequestStatus{Certificate: &issued, IssuedAt: lo.ToPtr(time.Now())})}).Error).To(Succeed())
	})
	AfterEach(func() { suite.Teardown() })
	makeHandler := func(validator policyservice.PolicySetValidator) http.Handler {
		policies := policyservice.NewServiceHandler(policystore.NewStore(suite.DB, suite.Log), validator, suite.cfg.CA.DeviceEnrollmentSignerName, nil, suite.Log)
		authorizer := policyAPIAuthorizer{static: authz.NewStaticAuthZ(suite.Log)}
		handler := transport.NewTransportHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, convert.NewConverter(), nil, nil, nil, authorizer, policies)
		router := chi.NewRouter()
		router.Use(auth.CreateAuthZMiddleware(authorizer, suite.Log))
		specification, err := api.GetSpec()
		Expect(err).NotTo(HaveOccurred())
		router.Use(oapimiddleware.OapiRequestValidatorWithOptions(specification, &oapimiddleware.Options{SilenceServersWarning: true}))
		return server.HandlerWithOptions(handler, server.ChiServerOptions{BaseURL: "/api/v1", BaseRouter: router})
	}
	request := func(handler http.Handler, method, path string, body []byte, ctx context.Context) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/v1/enrollmentapprovalpolicies"+path, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(response, req)
		return response
	}
	policyJSON := []byte(`{"apiVersion":"v1beta1","kind":"EnrollmentApprovalPolicy","metadata":{"name":"west"},"spec":{"certificateName":"commissioning","matchExpression":"true","deviceLabels":{}}}`)
	It("When real validation is unavailable it should return 503 without storing even a disabled draft", func() {
		handler := makeHandler(policyservice.UnavailablePolicySetValidator{})
		response := request(handler, http.MethodPost, "", policyJSON, suite.Ctx)
		Expect(response.Code).To(Equal(503), response.Body.String())
		Expect(response.Body.String()).To(ContainSubstring("PolicyValidationUnavailable"))
		Expect(request(handler, http.MethodGet, "/west", nil, suite.Ctx).Code).To(Equal(404))
	})
	It("When authorized lifecycle uses a test-only provider it should protect versions, bindings and status", func() {
		handler := makeHandler(policyAPIValidator{})
		response := request(handler, http.MethodPost, "", policyJSON, suite.Ctx)
		Expect(response.Code).To(Equal(201), response.Body.String())
		var created api.EnrollmentApprovalPolicy
		Expect(json.Unmarshal(response.Body.Bytes(), &created)).To(Succeed())
		Expect(created.Status).NotTo(BeNil())
		Expect(created.Status.EnrollmentCount).To(Equal(int64(0)))
		response = request(handler, http.MethodGet, "?fieldSelector=spec.unknown%3Dx", nil, suite.Ctx)
		Expect(response.Code).To(Equal(400), response.Body.String())
		response = request(handler, http.MethodGet, "/west/status", nil, suite.Ctx)
		Expect(response.Code).To(Equal(200))
		Expect(response.Body.String()).To(MatchJSON(`{"enrollmentCount":0}`))
		Expect(request(handler, http.MethodPut, "/west", policyJSON, suite.Ctx).Code).To(Equal(409))
		foreignContext := util.WithOrganizationID(suite.Ctx, uuid.New())
		Expect(request(handler, http.MethodGet, "/west", nil, foreignContext).Code).To(Equal(404))
		Expect(request(handler, http.MethodGet, "/west/status", nil, foreignContext).Code).To(Equal(404))
		created.Status = nil
		created.Spec.Enabled = true
		changed, err := json.Marshal(created)
		Expect(err).NotTo(HaveOccurred())
		response = request(handler, http.MethodPut, "/west", changed, suite.Ctx)
		Expect(response.Code).To(Equal(200), response.Body.String())
		Expect(json.Unmarshal(response.Body.Bytes(), &created)).To(Succeed())
		patch := []byte(`[{"op":"test","path":"/metadata/resourceVersion","value":"` + *created.Metadata.ResourceVersion + `"},{"op":"replace","path":"/spec/enabled","value":false}]`)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/enrollmentapprovalpolicies/west", bytes.NewReader(patch)).WithContext(suite.Ctx)
		req.Header.Set("Content-Type", "application/json-patch+json")
		patched := httptest.NewRecorder()
		handler.ServeHTTP(patched, req)
		Expect(patched.Code).To(Equal(200), patched.Body.String())
		Expect(json.Unmarshal(patched.Body.Bytes(), &created)).To(Succeed())
		created.Status = nil
		created.Spec.CertificateName = "other"
		changed, err = json.Marshal(created)
		Expect(err).NotTo(HaveOccurred())
		Expect(request(handler, http.MethodPut, "/west", changed, suite.Ctx).Code).To(Equal(409))
		organization := &model.Organization{ID: suite.OrgID}
		viewer := identity.NewMappedIdentity("viewer", "viewer", []*model.Organization{organization}, map[string][]string{suite.OrgID.String(): {api.RoleViewer}}, false, nil)
		viewerCtx := context.WithValue(suite.Ctx, consts.MappedIdentityCtxKey, viewer)
		Expect(request(handler, http.MethodPost, "", policyJSON, viewerCtx).Code).To(Equal(403))
		Expect(request(handler, http.MethodDelete, "/west", nil, suite.Ctx).Code).To(Equal(200))
		Expect(request(handler, http.MethodPost, "", policyJSON, suite.Ctx).Code).To(Equal(409))
	})
})
