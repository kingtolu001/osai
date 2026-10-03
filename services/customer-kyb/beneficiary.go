package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	BeneficiaryStatusActive     = "ACTIVE"
	BeneficiaryStatusDisabled   = "DISABLED"
	BeneficiaryApprovalPending  = "PENDING"
	BeneficiaryApprovalApproved = "APPROVED"
	BeneficiaryApprovalRejected = "REJECTED"
)

// Beneficiary is owned by Customer/KYB. Provider account resolution, when
// available, is evidence only and cannot alter this identity record.
type Beneficiary struct {
	ID             string    `json:"beneficiary_id"`
	InstitutionID  string    `json:"institution_id"`
	Name           string    `json:"name"`
	BankCode       string    `json:"bank_code"`
	AccountNumber  string    `json:"account_number"`
	Status         string    `json:"status"`
	ApprovalStatus string    `json:"approval_status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func bankDigits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func (s *Service) RegisterBeneficiary(institutionID, name, bankCode, accountNumber string, key ...string) (Beneficiary, error) {
	if err := s.ValidateInstitutionAccess(institutionID); err != nil {
		return Beneficiary{}, err
	}
	if strings.TrimSpace(name) == "" || !bankDigits(bankCode, 3, 6) || !bankDigits(accountNumber, 10, 10) {
		return Beneficiary{}, errors.New("invalid beneficiary details")
	}
	now := time.Now().UTC()
	beneficiary := Beneficiary{ID: "ben_" + uuid.NewString(), InstitutionID: institutionID, Name: strings.TrimSpace(name), BankCode: bankCode, AccountNumber: accountNumber, Status: BeneficiaryStatusActive, ApprovalStatus: BeneficiaryApprovalPending, CreatedAt: now, UpdatedAt: now}
	if len(key) > 0 && strings.TrimSpace(key[0]) != "" {
		beneficiary.ID = "ben_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(institutionID+"\x00"+key[0])).String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.beneficiaries[beneficiary.ID]; existing != nil {
		if existing.InstitutionID != institutionID || existing.Name != beneficiary.Name || existing.BankCode != bankCode || existing.AccountNumber != accountNumber {
			return Beneficiary{}, errors.New("beneficiary registration replay conflict")
		}
		return *existing, nil
	}
	if s.db != nil && len(key) > 0 && key[0] != "" {
		registered, err := s.insertBeneficiary(beneficiary)
		if err != nil {
			return Beneficiary{}, err
		}
		beneficiary = registered
	} else if err := s.saveBeneficiary(beneficiary); err != nil {
		return Beneficiary{}, err
	}
	s.beneficiaries[beneficiary.ID] = &beneficiary
	return beneficiary, nil
}

func (s *Service) insertBeneficiary(beneficiary Beneficiary) (Beneficiary, error) {
	raw, err := json.Marshal(beneficiary)
	if err != nil {
		return Beneficiary{}, err
	}
	if _, err = s.db.Exec(`INSERT INTO customer_beneficiaries(id,institution_id,body) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`, beneficiary.ID, beneficiary.InstitutionID, string(raw)); err != nil {
		return Beneficiary{}, err
	}
	var stored []byte
	if err = s.db.QueryRow(`SELECT body FROM customer_beneficiaries WHERE id=$1`, beneficiary.ID).Scan(&stored); err != nil {
		return Beneficiary{}, err
	}
	var existing Beneficiary
	if err = json.Unmarshal(stored, &existing); err != nil {
		return Beneficiary{}, err
	}
	if existing.InstitutionID != beneficiary.InstitutionID || existing.Name != beneficiary.Name || existing.BankCode != beneficiary.BankCode || existing.AccountNumber != beneficiary.AccountNumber {
		return Beneficiary{}, errors.New("beneficiary registration replay conflict")
	}
	return existing, nil
}

func (s *Service) GetApprovedBeneficiary(institutionID, beneficiaryID string) (Beneficiary, error) {
	if err := s.ValidateInstitutionAccess(institutionID); err != nil {
		return Beneficiary{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	beneficiary := s.beneficiaries[beneficiaryID]
	if beneficiary == nil || beneficiary.InstitutionID != institutionID {
		return Beneficiary{}, errors.New("beneficiary not found")
	}
	if beneficiary.Status != BeneficiaryStatusActive || beneficiary.ApprovalStatus != BeneficiaryApprovalApproved {
		return Beneficiary{}, errors.New("beneficiary not active and approved")
	}
	return *beneficiary, nil
}

func (s *Service) DisableBeneficiary(institutionID, beneficiaryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	beneficiary := s.beneficiaries[beneficiaryID]
	if beneficiary == nil || beneficiary.InstitutionID != institutionID {
		return errors.New("beneficiary not found")
	}
	copy := *beneficiary
	copy.Status = BeneficiaryStatusDisabled
	copy.UpdatedAt = time.Now().UTC()
	if err := s.saveBeneficiary(copy); err != nil {
		return err
	}
	*beneficiary = copy
	return nil
}

func (s *Service) saveBeneficiary(beneficiary Beneficiary) error {
	if s.db == nil {
		return nil
	}
	raw, err := json.Marshal(beneficiary)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO customer_beneficiaries(id,institution_id,body) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET body=EXCLUDED.body`, beneficiary.ID, beneficiary.InstitutionID, string(raw))
	return err
}
