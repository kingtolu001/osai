package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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

var defaultInstitutionEntitlements = []string{"quotes:read", "quotes:write"}

type Institution struct {
	ID           string    `json:"institution_id"`
	Name         string    `json:"business_name"`
	Status       string    `json:"status"`
	KYBStatus    string    `json:"kyb_status"`
	Country      string    `json:"country,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	APIEnabled   bool      `json:"api_access_enabled"`
	Entitlements []string  `json:"entitlements"`
}

type Credential struct {
	ID            string    `json:"credential_id"`
	PublicID      string    `json:"public_id"`
	InstitutionID string    `json:"institution_id"`
	Label         string    `json:"label"`
	Environment   string    `json:"environment"`
	Status        string    `json:"status"`
	SecretHash    string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CustomerWebhookConfig struct {
	InstitutionID string    `json:"institution_id"`
	WebhookURL    string    `json:"webhook_url"`
	SecretHash    string    `json:"secret_hash,omitempty"`
	Enabled       bool      `json:"enabled"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Service struct {
	db            *sql.DB
	mu            sync.Mutex
	institutions  map[string]*Institution
	credentials   map[string]*Credential
	publicToCred  map[string]*Credential
	beneficiaries map[string]*Beneficiary
	webhookCfg    map[string]*CustomerWebhookConfig
}

func NewService() *Service {
	return &Service{
		institutions:  make(map[string]*Institution),
		credentials:   make(map[string]*Credential),
		publicToCred:  make(map[string]*Credential),
		beneficiaries: make(map[string]*Beneficiary),
		webhookCfg:    make(map[string]*CustomerWebhookConfig),
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
		ID:           "inst_" + uuid.NewString(),
		Name:         name,
		Status:       status,
		KYBStatus:    KybStatusPending,
		Country:      country,
		CreatedAt:    now,
		UpdatedAt:    now,
		APIEnabled:   true,
		Entitlements: append([]string(nil), defaultInstitutionEntitlements...),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveInstitution(*inst); err != nil {
		return Institution{}, err
	}
	s.institutions[inst.ID] = inst
	created := *inst
	created.Entitlements = append([]string(nil), inst.Entitlements...)
	return created, nil
}

func (s *Service) GetInstitution(instID string) (Institution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.institutions[instID]
	if !ok {
		return Institution{}, false
	}
	copyInst := *inst
	copyInst.Entitlements = append([]string(nil), inst.Entitlements...)
	return copyInst, true
}

func (s *Service) SetInstitutionEntitlements(institutionID string, entitlements []string) error {
	institutionID = strings.TrimSpace(institutionID)
	if institutionID == "" {
		return errors.New("institution id required")
	}
	normalized := make([]string, 0, len(entitlements))
	seen := make(map[string]struct{}, len(entitlements))
	for _, entitlement := range entitlements {
		entitlement = strings.TrimSpace(entitlement)
		if entitlement == "" {
			return errors.New("entitlement cannot be empty")
		}
		if _, exists := seen[entitlement]; exists {
			continue
		}
		seen[entitlement] = struct{}{}
		normalized = append(normalized, entitlement)
	}
	sort.Strings(normalized)

	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.institutions[institutionID]
	if !ok {
		return errors.New("institution not found")
	}
	updated := *inst
	updated.Entitlements = normalized
	updated.UpdatedAt = time.Now().UTC()
	if err := s.saveInstitution(updated); err != nil {
		return err
	}
	*inst = updated
	return nil
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

func (s *Service) CreateCredential(instID, label string, environments ...string) (Credential, string, error) {
	if stringsTrim(label) == "" {
		label = "default"
	}
	environment := "sandbox"
	if len(environments) > 0 && stringsTrim(environments[0]) != "" {
		environment = stringsTrim(environments[0])
	}
	if environment != "sandbox" && environment != "production" {
		return Credential{}, "", errors.New("invalid credential environment")
	}
	publicID := "ck_" + uuid.NewString()
	secret := "secret_" + uuid.NewString()
	now := time.Now().UTC()
	cred := &Credential{
		ID:            "cred_" + uuid.NewString(),
		PublicID:      publicID,
		InstitutionID: instID,
		Label:         label,
		Environment:   environment,
		Status:        CredentialStatusActive,
		SecretHash:    hashSecret(secret, instID),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.institutions[instID]; !ok {
		return Credential{}, "", errors.New("institution not found")
	}
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
	updated := *cred
	updated.Status = CredentialStatusRevoked
	updated.UpdatedAt = time.Now().UTC()
	if err := s.saveCredential(updated); err != nil {
		return err
	}
	*cred = updated
	return nil
}

func (s *Service) RotateCredential(instID, publicID string) (Credential, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok || cred.InstitutionID != instID || cred.Status != CredentialStatusActive {
		return Credential{}, "", errors.New("credential not found")
	}
	secret := "secret_" + uuid.NewString()
	updated := *cred
	updated.SecretHash = hashSecret(secret, updated.InstitutionID)
	updated.UpdatedAt = time.Now().UTC()
	if err := s.saveCredential(updated); err != nil {
		return Credential{}, "", err
	}
	*cred = updated
	return updated, secret, nil
}

func (s *Service) RevokeInstitutionCredential(instID, publicID string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok || cred.InstitutionID != instID {
		return Credential{}, errors.New("credential not found")
	}
	updated := *cred
	updated.Status = CredentialStatusRevoked
	updated.UpdatedAt = time.Now().UTC()
	if err := s.saveCredential(updated); err != nil {
		return Credential{}, err
	}
	*cred = updated
	return updated, nil
}

func (s *Service) GetCredential(instID, publicID string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.publicToCred[publicID]
	if !ok || cred.InstitutionID != instID {
		return Credential{}, errors.New("credential not found")
	}
	metadata := *cred
	metadata.SecretHash = ""
	return metadata, nil
}

func (s *Service) ListCredentials(instID string) ([]Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.institutions[instID]; !ok {
		return nil, errors.New("institution not found")
	}
	result := make([]Credential, 0)
	for _, cred := range s.credentials {
		if cred.InstitutionID != instID {
			continue
		}
		metadata := *cred
		metadata.SecretHash = ""
		result = append(result, metadata)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].PublicID < result[j].PublicID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
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

func (s *Service) SetWebhookConfiguration(institutionID, webhookURL, secret string, enabled bool, status string) (CustomerWebhookConfig, error) {
	institutionID = strings.TrimSpace(institutionID)
	webhookURL = strings.TrimSpace(webhookURL)
	if institutionID == "" || webhookURL == "" {
		return CustomerWebhookConfig{}, errors.New("institution and webhook url required")
	}
	if status == "" {
		status = "ACTIVE"
	}
	if _, ok := s.GetInstitution(institutionID); !ok {
		return CustomerWebhookConfig{}, errors.New("institution not found")
	}
	secretValue := strings.TrimSpace(secret)
	if secretValue == "" {
		secretValue = "wh_" + uuid.NewString()
	}
	now := time.Now().UTC()
	cfg := CustomerWebhookConfig{
		InstitutionID: institutionID,
		WebhookURL:    webhookURL,
		SecretHash:    hashSecret(secretValue, institutionID),
		Enabled:       enabled,
		Status:        status,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if existing, ok := s.webhookCfg[institutionID]; ok {
		cfg.CreatedAt = existing.CreatedAt
		if cfg.CreatedAt.IsZero() {
			cfg.CreatedAt = now
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveWebhookConfiguration(cfg); err != nil {
		return CustomerWebhookConfig{}, err
	}
	s.webhookCfg[institutionID] = &cfg
	return cfg, nil
}

func (s *Service) GetWebhookConfiguration(institutionID string) (CustomerWebhookConfig, bool) {
	instID := strings.TrimSpace(institutionID)
	if instID == "" {
		return CustomerWebhookConfig{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.webhookCfg[instID]
	if !ok {
		return CustomerWebhookConfig{}, false
	}
	copyCfg := *cfg
	copyCfg.SecretHash = ""
	return copyCfg, true
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
