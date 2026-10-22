package main

import "time"

// Gateway response DTOs; persistence and authentication belong to Customer/KYB.
type CustomerInstitution struct {
	ID         string
	Name       string
	Status     string
	KYBStatus  string
	Country    string
	APIEnabled bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type CustomerCredential struct {
	ID            string
	PublicID      string
	InstitutionID string
	Label         string // Friendly name for the credential
	Environment   string // "sandbox" or "production"
	Status        string
	SecretHash    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
