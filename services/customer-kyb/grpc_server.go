package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"os"
	"strings"
	"time"

	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	customerv1.UnimplementedCustomerServiceServer
	service *Service
}

func (s *grpcServer) AuthenticateAPIClient(ctx context.Context, req *customerv1.CredentialAuthRequest) (*customerv1.AuthContext, error) {
	if s.service == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	ok, err := s.service.ValidateCredential(req.PublicId, req.Secret)
	if err != nil || !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	s.service.mu.Lock()
	cred, ok := s.service.publicToCred[req.PublicId]
	var institutionID string
	var environment string
	if ok && cred != nil {
		institutionID = cred.InstitutionID
		environment = cred.Environment
	}
	s.service.mu.Unlock()
	if !ok || cred == nil {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if environment == "" {
		environment = "sandbox"
	}
	inst, ok := s.service.GetInstitution(institutionID)
	if !ok {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	return &customerv1.AuthContext{InstitutionId: inst.ID, InstitutionStatus: inst.Status, KybStatus: inst.KYBStatus, ApiAccessStatus: "ACTIVE", ApiEnabled: inst.APIEnabled, Entitlements: inst.Entitlements, Environment: environment}, nil
}

func (s *grpcServer) CreateCredential(ctx context.Context, req *customerv1.CreateCredentialRequest) (*customerv1.CredentialResponse, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.Label) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id and label are required")
	}
	environment := strings.TrimSpace(req.Environment)
	if environment == "" {
		environment = "sandbox"
	}
	if environment != "sandbox" && environment != "production" {
		return nil, status.Error(codes.InvalidArgument, "environment is invalid")
	}
	credential, secret, err := s.service.CreateCredential(req.InstitutionId, req.Label, environment)
	if err != nil {
		if strings.Contains(err.Error(), "institution not found") {
			return nil, status.Error(codes.NotFound, "institution not found")
		}
		return nil, status.Error(codes.Internal, "credential could not be created")
	}
	return &customerv1.CredentialResponse{Credential: credentialToProto(credential), Secret: secret}, nil
}

func (s *grpcServer) RotateCredential(ctx context.Context, req *customerv1.CredentialRequest) (*customerv1.CredentialResponse, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.PublicId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id and public_id are required")
	}
	credential, secret, err := s.service.RotateCredential(req.InstitutionId, req.PublicId)
	if err != nil {
		if err.Error() != "credential not found" {
			return nil, status.Error(codes.Internal, "credential could not be rotated")
		}
		return nil, status.Error(codes.NotFound, "credential not found")
	}
	return &customerv1.CredentialResponse{Credential: credentialToProto(credential), Secret: secret}, nil
}

func (s *grpcServer) RevokeCredential(ctx context.Context, req *customerv1.CredentialRequest) (*customerv1.Credential, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.PublicId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id and public_id are required")
	}
	credential, err := s.service.RevokeInstitutionCredential(req.InstitutionId, req.PublicId)
	if err != nil {
		if err.Error() != "credential not found" {
			return nil, status.Error(codes.Internal, "credential could not be revoked")
		}
		return nil, status.Error(codes.NotFound, "credential not found")
	}
	return credentialToProto(credential), nil
}

func (s *grpcServer) GetCredential(ctx context.Context, req *customerv1.CredentialRequest) (*customerv1.Credential, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.PublicId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id and public_id are required")
	}
	credential, err := s.service.GetCredential(req.InstitutionId, req.PublicId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "credential not found")
	}
	return credentialToProto(credential), nil
}

func (s *grpcServer) ListCredentials(ctx context.Context, req *customerv1.InstitutionRequest) (*customerv1.CredentialList, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id is required")
	}
	credentials, err := s.service.ListCredentials(req.InstitutionId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	response := &customerv1.CredentialList{Credentials: make([]*customerv1.Credential, 0, len(credentials))}
	for _, credential := range credentials {
		response.Credentials = append(response.Credentials, credentialToProto(credential))
	}
	return response, nil
}

func credentialToProto(credential Credential) *customerv1.Credential {
	return &customerv1.Credential{
		CredentialId:  credential.ID,
		PublicId:      credential.PublicID,
		InstitutionId: credential.InstitutionID,
		Label:         credential.Label,
		Environment:   credential.Environment,
		Status:        credential.Status,
		CreatedAt:     credential.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     credential.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (s *grpcServer) GetInstitutionAccess(ctx context.Context, req *customerv1.InstitutionRequest) (*customerv1.AuthContext, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	inst, ok := s.service.GetInstitution(req.InstitutionId)
	if !ok {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	if err := s.service.ValidateInstitutionAccess(inst.ID); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &customerv1.AuthContext{InstitutionId: inst.ID, InstitutionStatus: inst.Status, KybStatus: inst.KYBStatus, ApiAccessStatus: "ACTIVE", ApiEnabled: inst.APIEnabled, Entitlements: inst.Entitlements}, nil
}

func (s *grpcServer) GetInstitution(ctx context.Context, req *customerv1.InstitutionRequest) (*customerv1.Institution, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	inst, ok := s.service.GetInstitution(req.InstitutionId)
	if !ok {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	return &customerv1.Institution{InstitutionId: inst.ID, Name: inst.Name, Status: inst.Status, KybStatus: inst.KYBStatus, Country: inst.Country, ApiEnabled: inst.APIEnabled}, nil
}

func (s *grpcServer) GetWebhookConfiguration(ctx context.Context, req *customerv1.InstitutionRequest) (*customerv1.WebhookConfiguration, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if _, ok := s.service.GetInstitution(req.InstitutionId); !ok {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	cfg, ok := s.service.GetWebhookConfiguration(req.InstitutionId)
	if !ok {
		return &customerv1.WebhookConfiguration{InstitutionId: req.InstitutionId, Enabled: false, Status: "INACTIVE"}, nil
	}
	return &customerv1.WebhookConfiguration{InstitutionId: cfg.InstitutionID, WebhookUrl: cfg.WebhookURL, Enabled: cfg.Enabled, Status: cfg.Status}, nil
}

func (s *grpcServer) GetApprovedBeneficiary(ctx context.Context, req *customerv1.BeneficiaryRequest) (*customerv1.Beneficiary, error) {
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.BeneficiaryId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution and beneficiary required")
	}
	beneficiary, err := s.service.GetApprovedBeneficiary(req.InstitutionId, req.BeneficiaryId)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "beneficiary unavailable")
	}
	return &customerv1.Beneficiary{BeneficiaryId: beneficiary.ID, InstitutionId: beneficiary.InstitutionID, Name: beneficiary.Name, BankCode: beneficiary.BankCode, AccountNumber: beneficiary.AccountNumber, Status: beneficiary.Status, ApprovalStatus: beneficiary.ApprovalStatus}, nil
}

func (s *grpcServer) RegisterBeneficiary(ctx context.Context, req *customerv1.RegisterBeneficiaryRequest) (*customerv1.Beneficiary, error) {
	secret := os.Getenv("OSAI_BENEFICIARY_WRITE_TOKEN")
	md, ok := metadata.FromIncomingContext(ctx)
	if secret == "" || !ok || len(md.Get("x-osai-beneficiary-write-token")) != 1 || subtle.ConstantTimeCompare([]byte(md.Get("x-osai-beneficiary-write-token")[0]), []byte(secret)) != 1 {
		return nil, status.Error(codes.PermissionDenied, "beneficiary registration unavailable")
	}
	if s.service == nil || req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution and beneficiary details required")
	}
	beneficiary, err := s.service.RegisterBeneficiary(req.InstitutionId, req.Name, req.BankCode, req.AccountNumber, req.IdempotencyKey)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "beneficiary registration unavailable")
	}
	return &customerv1.Beneficiary{BeneficiaryId: beneficiary.ID, InstitutionId: beneficiary.InstitutionID, Name: beneficiary.Name, BankCode: beneficiary.BankCode, AccountNumber: beneficiary.AccountNumber, Status: beneficiary.Status, ApprovalStatus: beneficiary.ApprovalStatus}, nil
}

func (s *Service) ValidateAPIClient(publicID, secret string) (Institution, error) {
	ok, err := s.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return Institution{}, errors.New("invalid credentials")
	}
	cred := s.publicToCred[publicID]
	inst, ok := s.GetInstitution(cred.InstitutionID)
	if !ok {
		return Institution{}, errors.New("institution not found")
	}
	if err := s.ValidateInstitutionAccess(inst.ID); err != nil {
		return Institution{}, err
	}
	return inst, nil
}
