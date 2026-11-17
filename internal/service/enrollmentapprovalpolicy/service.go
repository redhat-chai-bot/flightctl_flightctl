package enrollmentapprovalpolicy

import (
	"context"

	"github.com/flightctl/flightctl/internal/domain"
	"github.com/google/uuid"
)

type Service interface {
	CreateEnrollmentApprovalPolicy(context.Context, uuid.UUID, domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, domain.Status)
	ListEnrollmentApprovalPolicies(context.Context, uuid.UUID, domain.ListEnrollmentApprovalPoliciesParams) (*domain.EnrollmentApprovalPolicyList, domain.Status)
	GetEnrollmentApprovalPolicy(context.Context, uuid.UUID, string) (*domain.EnrollmentApprovalPolicy, domain.Status)
	ReplaceEnrollmentApprovalPolicy(context.Context, uuid.UUID, string, domain.EnrollmentApprovalPolicy) (*domain.EnrollmentApprovalPolicy, domain.Status)
	PatchEnrollmentApprovalPolicy(context.Context, uuid.UUID, string, domain.PatchRequest) (*domain.EnrollmentApprovalPolicy, domain.Status)
	DeleteEnrollmentApprovalPolicy(context.Context, uuid.UUID, string) domain.Status
	GetEnrollmentApprovalPolicyStatus(context.Context, uuid.UUID, string) (*domain.EnrollmentApprovalPolicyStatus, domain.Status)
}
