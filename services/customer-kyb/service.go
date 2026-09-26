package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	InstitutionStatusActive    = "ACTIVE"
	InstitutionStatusDisabled  = "DISABLED"
	InstitutionStatusSuspended = "SUSPENDED"
	InstitutionStatusClosed    = "CLOSED"
	KybStatusPending           = "PENDING"
	KybStatusApproved          = "APPROVED"
	KybStatusRejected          = "REJECTED"
	KybStatusExpired           = "EXPIRED"
	CredentialStatusActive     = "ACTIVE"
	CredentialStatusRevoked    = "REVOKED"
	CredentialStatusExpired    = "EXPIRED"
)

type Institution struct {
	ID         string    `json:"institution_id"`
	Name       string    `json:"business_name"`
	Status     string    `json:"status"`
	KYBStatus  string    `json:"kyb_status"`
	Country    string    `json:"country,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	APIEnabled bool      `json:"api_access_enabled"`
}

type Credential struct {
	ID            string    `json:"credential_id"`
	PublicID      string    `json:"public_id"`
	InstitutionID string    `json:"institution_id"`
	Status        string    `json:"status"`
	SecretHash    string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Service struct {
	db           *sql.DB
	mu           sync.Mutex
	institutions map[string]*Institution
	credentials  map[string]*Credential
	publicToCred map[string]*Credential
}

func NewService() *Service {
	return &Service{
		institutions: make(map[string]*Institution),
		credentials:  make(map[string]*Credential),
		publicToCred: make(map[string]*Credential),
	}
}

func (s *Service) CreateInstitution(name, country, status string) (Institution, error) {
	if stringsTrim(name) == "" {
		return Institution{}, errors.New("institution name required")
	}
	if status == "" {
		status = InstitutionStatusActive
	}
	now := time.Now().UTC()
	inst := &Institution{
		ID:         "inst_" + uuid.NewString(),
		Name:       name,
		Status:     status,
		KYBStatus:  KybStatusPending,
		Country:    country,
		CreatedAt:  now,
		UpdatedAt:  now,
		APIEnabled: true,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveInstitution(*inst); err != nil {
		return Institution{}, err
	}
	s.institutions[inst.ID] = inst
	return *inst, nil
}

func (s *Service) GetInstitution(instID string) (Institution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok {
		return Institution{}, false
	}
	copyInst := *inst
	return copyInst, true
}

func (s *Service) InstitutionAllowed(instID string) (Institution, bool, error) {
	inst, ok := s.GetInstitution(instID)
	if !ok {
		return Institution{}, false, errors.New("institution not found")
	}
	if inst.Status != InstitutionStatusActive {
		return inst, false, nil
	}
	if inst.APIEnabled == false {
		return inst, false, nil
	}
	return inst, true, nil
}

func (s *Service) CreateCredential(instID, label string) (Credential, string, error) {
	if stringsTrim(label) == "" {
		label = "default"
	}
	if _, ok := s.GetInstitution(instID); !ok {
		return Credential{}, "", errors.New("institution not found")
	}
	publicID := "ck_" + uuid.NewString()
	secret := "secret_" + uuid.NewString()
	now := time.Now().UTC()
	cred := &Credential{
		ID:            "cred_" + uuid.NewString(),
		PublicID:      publicID,
		InstitutionID: instID,
		Status:        CredentialStatusActive,
		SecretHash:    hashSecret(secret, instID),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveCredential(*cred); err != nil {
		return Credential{}, "", err
	}
	s.credentials[cred.ID] = cred
	s.publicToCred[cred.PublicID] = cred
	return *cred, secret, nil
}

func (s *Service) ValidateCredential(publicID, secret string) (bool, error) {
	if stringsTrim(publicID) == "" || stringsTrim(secret) == "" {
		return false, errors.New("missing credential material")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return false, errors.New("invalid credentials")
	}
	if cred.Status != CredentialStatusActive {
		return false, errors.New("credential revoked or expired")
	}
	expected := hashSecret(secret, cred.InstitutionID)
	return hmac.Equal([]byte(cred.SecretHash), []byte(expected)), nil
}

func (s *Service) RevokeCredential(publicID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok {
		return errors.New("credential not found")
	}
	cred.Status = CredentialStatusRevoked
	cred.UpdatedAt = time.Now().UTC()
	if err := s.saveCredential(*cred); err != nil {
		return err
	}
	return nil
}

func hashSecret(secret, instID string) string {
	key := []byte(secret + ":" + instID)
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

func stringsTrim(s string) string { return strings.TrimSpace(s) }

func (s *Service) APIKeyForInstitution(publicID, secret string) (string, error) {
	ok, err := s.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return "", fmt.Errorf("invalid credentials")
	}
	cred := s.publicToCred[publicID]
	return cred.InstitutionID, nil
}

func (s *Service) ValidateInstitutionAccess(instID string) error {
	inst, ok := s.GetInstitution(instID)
	if !ok {
		return errors.New("customer not found")
	}
	if inst.Status != InstitutionStatusActive {
		return errors.New("customer disabled")
	}
	if !inst.APIEnabled {
		return errors.New("api access disabled")
	}
	if inst.KYBStatus != KybStatusApproved {
		return errors.New("kyb not approved")
	}
	return nil
}
