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

type CustomerInstitution struct {
	ID          string
	Name        string
	Status      string
	KYBStatus   string
	Country     string
	APIEnabled  bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CustomerCredential struct {
	ID            string
	PublicID      string
	InstitutionID string
	Status        string
	SecretHash    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type CustomerService struct {
	mu          sync.Mutex
	institutions map[string]*CustomerInstitution
	publicToCred map[string]*CustomerCredential
}

func NewCustomerService() *CustomerService {
	return &CustomerService{institutions: map[string]*CustomerInstitution{}, publicToCred: map[string]*CustomerCredential{}}
}

func (s *CustomerService) GetInstitution(instID string) (CustomerInstitution, bool) {
	s.mu.Lock(); defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok { return CustomerInstitution{}, false }
	copyInst := *inst
	return copyInst, true
}

func (s *CustomerService) CreateInstitution(name, country, status string) (CustomerInstitution, error) {
	if strings.TrimSpace(name) == "" { return CustomerInstitution{}, errors.New("institution name required") }
	if status == "" { status = "ACTIVE" }
	now := time.Now().UTC()
	inst := &CustomerInstitution{ID: "inst_" + uuid.NewString(), Name: name, Status: status, KYBStatus: "APPROVED", Country: country, APIEnabled: true, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock(); defer s.mu.Unlock(); s.institutions[inst.ID] = inst; return *inst, nil
}

func (s *CustomerService) CreateCredential(instID, label string) (CustomerCredential, string, error) {
	if strings.TrimSpace(label) == "" { label = "default" }
	s.mu.Lock(); defer s.mu.Unlock()
	if _, ok := s.institutions[instID]; !ok { return CustomerCredential{}, "", errors.New("institution not found") }
	publicID := "ck_" + uuid.NewString()
	secret := "secret_" + uuid.NewString()
	cred := &CustomerCredential{ID: "cred_" + uuid.NewString(), PublicID: publicID, InstitutionID: instID, Status: "ACTIVE", SecretHash: hashCustomerSecret(secret, instID), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.publicToCred[publicID] = cred
	return *cred, secret, nil
}

func (s *CustomerService) ValidateCredential(publicID, secret string) (bool, error) {
	if strings.TrimSpace(publicID) == "" || strings.TrimSpace(secret) == "" { return false, errors.New("missing credentials") }
	s.mu.Lock(); defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok { return false, errors.New("invalid credentials") }
	if cred.Status != "ACTIVE" { return false, errors.New("revoked credentials") }
	return hmac.Equal([]byte(cred.SecretHash), []byte(hashCustomerSecret(secret, cred.InstitutionID))), nil
}

func (s *CustomerService) ValidateInstitutionAccess(instID string) error {
	s.mu.Lock(); defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok { return errors.New("customer disabled") }
	if inst.Status != "ACTIVE" { return errors.New("customer disabled") }
	if !inst.APIEnabled { return errors.New("customer disabled") }
	if inst.KYBStatus != "APPROVED" { return errors.New("kyb status invalid") }
	return nil
}

func (s *CustomerService) InstitutionForAPIKey(publicID, secret string) (string, error) {
	ok, err := s.ValidateCredential(publicID, secret)
	if err != nil || !ok { return "", fmt.Errorf("invalid credentials") }
	cred := s.publicToCred[publicID]
	if cred == nil { return "", errors.New("invalid credentials") }
	return cred.InstitutionID, nil
}

func hashCustomerSecret(secret, instID string) string {
	sum := sha256.Sum256([]byte(secret + ":" + instID))
	return hex.EncodeToString(sum[:])
}
