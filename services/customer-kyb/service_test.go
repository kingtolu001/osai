package main

import (
	"os"
	"testing"
)

func TestCustomerServiceCreatesInstitutionAndCredential(t *testing.T) {
	svc := NewService()
	i, err := svc.CreateInstitution("Acme Treasury", "US", "ACTIVE")
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}
	if i.ID == "" || i.Name == "" {
		t.Fatal("institution should be persisted with id and name")
	}

	cred, secret, err := svc.CreateCredential(i.ID, "sandbox")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if cred.PublicID == "" || secret == "" {
		t.Fatal("credential creation must return public id and secret")
	}
	if cred.InstitutionID != i.ID {
		t.Fatalf("credential should map to institution %s but got %s", i.ID, cred.InstitutionID)
	}
	if cred.Status != CredentialStatusActive {
		t.Fatalf("new credential status should be active, got %s", cred.Status)
	}
	if cred.SecretHash == secret {
		t.Fatal("plaintext secret must not be stored")
	}

	if ok, err := svc.ValidateCredential(cred.PublicID, secret); err != nil || !ok {
		t.Fatalf("credential validation should succeed for valid secret: ok=%v err=%v", ok, err)
	}

	if err := svc.RevokeCredential(cred.PublicID); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	if ok, _ := svc.ValidateCredential(cred.PublicID, secret); ok {
		t.Fatal("revoked credential should fail validation")
	}
}

func TestSeedSandboxCustomerUsesStableInstitutionID(t *testing.T) {
	t.Setenv("OSAI_SANDBOX_SEED", "1")
	t.Setenv("OSAI_SANDBOX_INSTITUTION_ID", "")
	t.Setenv("OSAI_SANDBOX_CLIENT_ID", "")
	t.Setenv("OSAI_SANDBOX_CLIENT_SECRET", "local-test-secret")

	svc := NewService()
	if err := seedSandboxCustomer(svc); err != nil {
		t.Fatalf("seed sandbox customer: %v", err)
	}

	if _, ok := svc.GetInstitution("inst_sandbox_local"); !ok {
		t.Fatal("sandbox institution should be created as inst_sandbox_local")
	}

	if got := os.Getenv("OSAI_SANDBOX_INSTITUTION_ID"); got != "inst_sandbox_local" {
		t.Fatalf("sandbox institution id should remain stable, got %q", got)
	}
}
