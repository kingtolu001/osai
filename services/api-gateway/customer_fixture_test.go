package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type CustomerService struct {
	mu           sync.Mutex
	institutions map[string]*CustomerInstitution
	publicToCred map[string]*CustomerCredential
}

func NewCustomerService() *CustomerService {
	return &CustomerService{institutions: map[string]*CustomerInstitution{}, publicToCred: map[string]*CustomerCredential{}}
}

func (s *CustomerService) GetInstitution(instID string) (CustomerInstitution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok {
		return CustomerInstitution{}, false
	}
	copyInst := *inst
	return copyInst, true
}

func (s *CustomerService) CreateInstitution(name, country, status string) (CustomerInstitution, error) {
	if strings.TrimSpace(name) == "" {
		return CustomerInstitution{}, errors.New("institution name required")
	}
	if status == "" {
		status = "ACTIVE"
	}
	now := time.Now().UTC()
	inst := &CustomerInstitution{ID: "inst_" + uuid.NewString(), Name: name, Status: status, KYBStatus: "APPROVED", Country: country, APIEnabled: true, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.institutions[inst.ID] = inst
	return *inst, nil
}

func (s *CustomerService) CreateCredential(instID, label, environment string) (CustomerCredential, string, error) {
	if strings.TrimSpace(label) == "" {
		label = "default"
	}
	if environment == "" {
		environment = "sandbox"
	}
	if environment != "sandbox" && environment != "production" {
		return CustomerCredential{}, "", errors.New("invalid environment; must be 'sandbox' or 'production'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.institutions[instID]; !ok {
		return CustomerCredential{}, "", errors.New("institution not found")
	}
	publicID := "ck_" + uuid.NewString()
	secret := "secret_" + uuid.NewString()
	cred := &CustomerCredential{ID: "cred_" + uuid.NewString(), PublicID: publicID, InstitutionID: instID, Label: label, Environment: environment, Status: "ACTIVE", SecretHash: hashCustomerSecret(secret, instID), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.publicToCred[publicID] = cred
	return *cred, secret, nil
}

func (s *CustomerService) ValidateCredential(publicID, secret string) (bool, error) {
	if strings.TrimSpace(publicID) == "" || strings.TrimSpace(secret) == "" {
		return false, errors.New("missing credentials")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return false, errors.New("invalid credentials")
	}
	if cred.Status != "ACTIVE" {
		return false, errors.New("revoked credentials")
	}
	return hmac.Equal([]byte(cred.SecretHash), []byte(hashCustomerSecret(secret, cred.InstitutionID))), nil
}

func (s *CustomerService) ValidateInstitutionAccess(instID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok {
		return errors.New("customer disabled")
	}
	if inst.Status != "ACTIVE" {
		return errors.New("customer disabled")
	}
	if !inst.APIEnabled {
		return errors.New("customer disabled")
	}
	if inst.KYBStatus != "APPROVED" {
		return errors.New("kyb status invalid")
	}
	return nil
}

func (s *CustomerService) InstitutionForAPIKey(publicID, secret string) (string, error) {
	ok, err := s.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return "", fmt.Errorf("invalid credentials")
	}
	cred := s.publicToCred[publicID]
	if cred == nil {
		return "", errors.New("invalid credentials")
	}
	return cred.InstitutionID, nil
}

// RotateCredential returns a new secret while keeping the same public ID.
// The old secret becomes invalid immediately.
func (s *CustomerService) RotateCredential(instID, publicID string) (CustomerCredential, string, error) {
	if strings.TrimSpace(publicID) == "" {
		return CustomerCredential{}, "", errors.New("public_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return CustomerCredential{}, "", errors.New("credential not found")
	}
	if cred.InstitutionID != instID {
		return CustomerCredential{}, "", errors.New("credential not found")
	}
	if cred.Status != "ACTIVE" {
		return CustomerCredential{}, "", errors.New("credential not active")
	}
	newSecret := "secret_" + uuid.NewString()
	cred.SecretHash = hashCustomerSecret(newSecret, cred.InstitutionID)
	cred.UpdatedAt = time.Now().UTC()
	copyCred := *cred
	return copyCred, newSecret, nil
}

// RevokeCredential marks a credential as revoked and invalid.
func (s *CustomerService) RevokeCredential(instID, publicID string) error {
	if strings.TrimSpace(publicID) == "" {
		return errors.New("public_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return errors.New("credential not found")
	}
	if cred.InstitutionID != instID {
		return errors.New("credential not found")
	}
	cred.Status = "REVOKED"
	cred.UpdatedAt = time.Now().UTC()
	return nil
}

// GetCredential returns credential metadata (not the secret).
func (s *CustomerService) GetCredential(instID, publicID string) (*CustomerCredential, error) {
	if strings.TrimSpace(publicID) == "" {
		return nil, errors.New("public_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return nil, errors.New("credential not found")
	}
	if cred.InstitutionID != instID {
		return nil, errors.New("credential not found")
	}
	return cred, nil
}

// ListCredentials returns all credentials for an institution (metadata only).
func (s *CustomerService) ListCredentials(instID string) ([]*CustomerCredential, error) {
	if strings.TrimSpace(instID) == "" {
		return nil, errors.New("institution_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.institutions[instID]; !ok {
		return nil, errors.New("institution not found")
	}
	var result []*CustomerCredential
	for _, cred := range s.publicToCred {
		if cred.InstitutionID == instID {
			result = append(result, cred)
		}
	}
	return result, nil
}

func hashCustomerSecret(secret, instID string) string {
	sum := sha256.Sum256([]byte(secret + ":" + instID))
	return hex.EncodeToString(sum[:])
}
