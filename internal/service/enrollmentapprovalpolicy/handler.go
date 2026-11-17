package enrollmentapprovalpolicy

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"time"

	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/flterrors"
	"github.com/flightctl/flightctl/internal/service/common"
	"github.com/flightctl/flightctl/internal/service/events"
	policystore "github.com/flightctl/flightctl/internal/store/enrollmentapprovalpolicy"
	"github.com/flightctl/flightctl/internal/store/selector"
	"github.com/flightctl/flightctl/pkg/jsonpatch"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
)

type ServiceHandler struct {
	store      policystore.Store
	validator  PolicySetValidator
	signerName string
	events     events.Service
	log        logrus.FieldLogger
}

func NewServiceHandler(policies policystore.Store, validator PolicySetValidator, signerName string, eventService events.Service, log logrus.FieldLogger) *ServiceHandler {
	if policies == nil || validator == nil || signerName == "" {
		panic("policy store, validator and enrollment signer are required")
	}
	return &ServiceHandler{store: policies, validator: validator, signerName: signerName, events: eventService, log: log}
}

func failure(code int32, reason, message string) domain.Status {
	return domain.NewFailureStatus(code, reason, message)
}

type policyError struct{ status domain.Status }

func (problem policyError) Error() string { return problem.status.Message }

func invalid(reason, message string) error { return policyError{failure(400, reason, message)} }

func mutationStatus(err error, name string, created bool) domain.Status {
	if err == nil {
		if created {
			return domain.StatusCreated()
		}
		return domain.StatusOK()
	}
	var problem policyError
	var overlap PolicyOverlapError
	var expression InvalidExpressionError
	var selectorFailure *selector.SelectorError
	switch {
	case errors.As(err, &problem):
		return problem.status
	case errors.Is(err, ErrValidationUnavailable):
		return failure(503, "PolicyValidationUnavailable", err.Error())
	case errors.As(err, &overlap):
		return failure(409, "PolicyOverlap", overlap.Error())
	case errors.As(err, &expression):
		return failure(400, "InvalidPolicy", expression.Error())
	case errors.Is(err, flterrors.ErrImmutableCertificateBinding):
		return failure(409, "ImmutableCertificateBinding", err.Error())
	case errors.Is(err, flterrors.ErrResourceVersionConflict):
		return failure(409, "ResourceVersionConflict", err.Error())
	case errors.Is(err, flterrors.ErrDuplicateName):
		return failure(409, "AlreadyExists", err.Error())
	case errors.Is(err, flterrors.ErrResourceNotFound):
		return failure(404, "NotFound", "policy or enrollment credential not found")
	case errors.As(err, &selectorFailure), errors.Is(err, flterrors.ErrFieldSelectorUnknownSelector):
		return domain.StatusBadRequest(err.Error())
	default:
		status := common.StoreErrorToApiStatus(err, created, domain.EnrollmentApprovalPolicyKind, &name)
		if status.Code == 400 {
			return status
		}
		return failure(503, "PolicyStateUnavailable", "policy transaction could not be committed")
	}
}

func (handler *ServiceHandler) credentialValid(credential *domain.CertificateSigningRequest, candidate *domain.EnrollmentApprovalPolicy) error {
	if credential.Spec.SignerName != handler.signerName {
		return invalid("InvalidPolicy", "certificateName must select an enrollment credential")
	}
	if credential.Status == nil || credential.Status.Certificate == nil || credential.Status.Revocation != nil {
		return invalid("InvalidPolicy", "enrollment credential must be issued and unrevoked")
	}
	block, _ := pem.Decode(*credential.Status.Certificate)
	if block == nil || block.Type != "CERTIFICATE" {
		return invalid("InvalidPolicy", "enrollment credential certificate is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return invalid("InvalidPolicy", "enrollment credential certificate is invalid")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return invalid("InvalidPolicy", "enrollment credential must be currently valid")
	}
	if candidate != nil && candidate.Spec.AutoApprovalWindowDays != nil && credential.Status.IssuedAt == nil {
		return invalid("InvalidPolicy", "window requires authoritative certificate issuance time")
	}
	return nil
}

func (handler *ServiceHandler) mutate(ctx context.Context, orgID uuid.UUID, name, certificate string, operation policystore.Operation, prepare func(*domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error)) (*domain.EnrollmentApprovalPolicy, domain.Status) {
	var previousMetadata domain.ObjectMeta
	result, err := handler.store.Mutate(ctx, orgID, name, certificate, operation, func(current *domain.EnrollmentApprovalPolicy, credential *domain.CertificateSigningRequest, enabled []domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error) {
		if current != nil {
			previousMetadata = current.Metadata
		}
		candidate, err := prepare(current)
		if err != nil {
			return nil, err
		}
		if err := handler.credentialValid(credential, candidate); err != nil {
			return nil, err
		}
		if candidate != nil {
			if errs := candidate.Validate(); len(errs) > 0 {
				return nil, invalid("InvalidPolicy", errors.Join(errs...).Error())
			}
			if candidate.Spec.Enabled {
				enabled = append(enabled, *candidate)
			}
		}
		if err := handler.validator.ValidatePolicySet(ctx, candidate, enabled); err != nil {
			return nil, err
		}
		return candidate, nil
	})
	if err == nil && handler.events != nil {
		common.SafeEventCallback(handler.log, func() {
			if operation == policystore.Delete {
				handler.events.HandleGenericResourceDeletedEvents(ctx, domain.EnrollmentApprovalPolicyKind, orgID, name, nil, nil, false, nil)
				return
			}
			details := common.ComputeResourceUpdatedDetails(previousMetadata, result.Metadata)
			handler.events.CreateEvent(ctx, orgID, common.GetResourceCreatedOrUpdatedSuccessEvent(ctx, operation == policystore.Create, domain.EnrollmentApprovalPolicyKind, name, details, handler.log, nil))
		})
	}
	return result, mutationStatus(err, name, operation == policystore.Create)
}

func (handler *ServiceHandler) CreateEnrollmentApprovalPolicy(ctx context.Context, orgID uuid.UUID, policy domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, domain.Status) {
	if policy.Status != nil {
		return nil, failure(400, "ProtectedField", "status is server-owned")
	}
	if errs := policy.Validate(); len(errs) > 0 {
		return nil, failure(400, "InvalidPolicy", errors.Join(errs...).Error())
	}
	if policy.Metadata.ResourceVersion != nil {
		return nil, failure(400, "InvalidPolicy", "resourceVersion must be absent on create")
	}
	common.NilOutManagedObjectMetaProperties(&policy.Metadata)
	return handler.mutate(ctx, orgID, *policy.Metadata.Name, policy.Spec.CertificateName, policystore.Create, func(*domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error) { return &policy, nil })
}

func (handler *ServiceHandler) GetEnrollmentApprovalPolicy(ctx context.Context, orgID uuid.UUID, name string) (*domain.EnrollmentApprovalPolicy, domain.Status) {
	result, err := handler.store.Get(ctx, orgID, name)
	return result, mutationStatus(err, name, false)
}

func (handler *ServiceHandler) ListEnrollmentApprovalPolicies(ctx context.Context, orgID uuid.UUID, params domain.ListEnrollmentApprovalPoliciesParams) (*domain.EnrollmentApprovalPolicyList, domain.Status) {
	listParams, status := common.PrepareListParams(params.Continue, params.LabelSelector, params.FieldSelector, params.Limit)
	if status.Code != 200 {
		return nil, status
	}
	result, err := handler.store.List(ctx, orgID, *listParams)
	return result, mutationStatus(err, "", false)
}

func (handler *ServiceHandler) GetEnrollmentApprovalPolicyStatus(ctx context.Context, orgID uuid.UUID, name string) (*domain.EnrollmentApprovalPolicyStatus, domain.Status) {
	policy, status := handler.GetEnrollmentApprovalPolicy(ctx, orgID, name)
	if policy == nil {
		return nil, status
	}
	return policy.Status, status
}

func (handler *ServiceHandler) ReplaceEnrollmentApprovalPolicy(ctx context.Context, orgID uuid.UUID, name string, policy domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, domain.Status) {
	if policy.Status != nil {
		return nil, failure(400, "ProtectedField", "status is server-owned")
	}
	if policy.Metadata.Name == nil || *policy.Metadata.Name != name {
		return nil, failure(400, "InvalidPolicy", "metadata.name must match the path")
	}
	if policy.Metadata.ResourceVersion == nil {
		return nil, failure(409, "ResourceVersionConflict", "current resourceVersion is required")
	}
	current, status := handler.GetEnrollmentApprovalPolicy(ctx, orgID, name)
	if current == nil {
		return nil, status
	}
	return handler.mutate(ctx, orgID, name, current.Spec.CertificateName, policystore.Update, func(stored *domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error) {
		if *policy.Metadata.ResourceVersion != lo.FromPtr(stored.Metadata.ResourceVersion) {
			return nil, flterrors.ErrResourceVersionConflict
		}
		if policy.Spec.CertificateName != stored.Spec.CertificateName {
			return nil, flterrors.ErrImmutableCertificateBinding
		}
		common.NilOutManagedObjectMetaProperties(&policy.Metadata)
		return &policy, nil
	})
}

func (handler *ServiceHandler) PatchEnrollmentApprovalPolicy(ctx context.Context, orgID uuid.UUID, name string, patch domain.PatchRequest) (*domain.EnrollmentApprovalPolicy, domain.Status) {
	version := ""
	for _, operation := range patch {
		if operation.Path == "/metadata/resourceVersion" && string(operation.Op) == "test" {
			version, _ = operation.Value.(string)
			continue
		}
		if !strings.HasPrefix(operation.Path, "/spec/") && operation.Path != "/spec" && !strings.HasPrefix(operation.Path, "/metadata/labels/") && operation.Path != "/metadata/labels" && !strings.HasPrefix(operation.Path, "/metadata/annotations/") && operation.Path != "/metadata/annotations" {
			return nil, failure(400, "ProtectedField", "patch may only change spec, labels or annotations")
		}
	}
	if version == "" {
		return nil, failure(409, "ResourceVersionConflict", "patch requires a resourceVersion test")
	}
	current, status := handler.GetEnrollmentApprovalPolicy(ctx, orgID, name)
	if current == nil {
		return nil, status
	}
	return handler.mutate(ctx, orgID, name, current.Spec.CertificateName, policystore.Update, func(stored *domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error) {
		if version != lo.FromPtr(stored.Metadata.ResourceVersion) {
			return nil, flterrors.ErrResourceVersionConflict
		}
		stored.Status = nil
		var candidate domain.EnrollmentApprovalPolicy
		if err := jsonpatch.Apply(*stored, &candidate, patch); err != nil {
			return nil, invalid("InvalidPolicy", err.Error())
		}
		if candidate.Spec.CertificateName != stored.Spec.CertificateName {
			return nil, flterrors.ErrImmutableCertificateBinding
		}
		return &candidate, nil
	})
}

func (handler *ServiceHandler) DeleteEnrollmentApprovalPolicy(ctx context.Context, orgID uuid.UUID, name string) domain.Status {
	current, status := handler.GetEnrollmentApprovalPolicy(ctx, orgID, name)
	if current == nil {
		return status
	}
	_, status = handler.mutate(ctx, orgID, name, current.Spec.CertificateName, policystore.Delete, func(*domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, error) { return nil, nil })
	return status
}

var _ Service = (*ServiceHandler)(nil)
