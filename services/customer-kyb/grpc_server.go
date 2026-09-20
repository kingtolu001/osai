package main

import (
	"context"
	"errors"
	"strings"

	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"google.golang.org/grpc/codes"
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
	cred, ok := s.service.publicToCred[req.PublicId]
	if !ok || cred == nil {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	inst, ok := s.service.GetInstitution(cred.InstitutionID)
	if !ok {
		return nil, status.Error(codes.NotFound, "institution not found")
	}
	return &customerv1.AuthContext{InstitutionId: inst.ID, InstitutionStatus: inst.Status, KybStatus: inst.KYBStatus, ApiAccessStatus: "ACTIVE", ApiEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
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
	return &customerv1.AuthContext{InstitutionId: inst.ID, InstitutionStatus: inst.Status, KybStatus: inst.KYBStatus, ApiAccessStatus: "ACTIVE", ApiEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
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
	return &customerv1.WebhookConfiguration{InstitutionId: req.InstitutionId, WebhookUrl: "https://example.invalid/webhook", Enabled: true, Status: "ACTIVE"}, nil
}

func (s *Service) ValidateAPIClient(publicID, secret string) (Institution, error) {
	ok, err := s.ValidateCredential(publicID, secret)
	if err != nil || !ok { return Institution{}, errors.New("invalid credentials") }
	cred := s.publicToCred[publicID]
	inst, ok := s.GetInstitution(cred.InstitutionID)
	if !ok { return Institution{}, errors.New("institution not found") }
	if err := s.ValidateInstitutionAccess(inst.ID); err != nil { return Institution{}, err }
	return inst, nil
}
