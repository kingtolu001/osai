package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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

func TestCredentialLifecycleIsInstitutionScopedAndEnvironmentBound(t *testing.T) {
	svc := NewService()
	first, err := svc.CreateInstitution("Credential Owner", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateInstitution("Other Institution", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}

	credential, oldSecret, err := svc.CreateCredential(first.ID, "production-key", "production")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Label != "production-key" || credential.Environment != "production" {
		t.Fatalf("credential metadata not preserved: %+v", credential)
	}
	if _, _, err := svc.CreateCredential(first.ID, "bad-environment", "staging"); err == nil {
		t.Fatal("unsupported credential environment was accepted")
	}
	if _, err := svc.GetCredential(second.ID, credential.PublicID); err == nil {
		t.Fatal("cross-institution credential lookup should not succeed")
	}
	if _, err := svc.RevokeInstitutionCredential(second.ID, credential.PublicID); err == nil {
		t.Fatal("cross-institution credential revocation should not succeed")
	}

	listed, err := svc.ListCredentials(first.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list credentials: count=%d err=%v", len(listed), err)
	}
	if listed[0].SecretHash != "" {
		t.Fatal("credential listing exposed secret hash")
	}
	rotated, newSecret, err := svc.RotateCredential(first.ID, credential.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Environment != "production" || rotated.Label != "production-key" {
		t.Fatalf("rotation changed immutable metadata: %+v", rotated)
	}
	if valid, _ := svc.ValidateCredential(credential.PublicID, oldSecret); valid {
		t.Fatal("rotated secret should no longer authenticate")
	}
	if valid, err := svc.ValidateCredential(credential.PublicID, newSecret); err != nil || !valid {
		t.Fatalf("new secret should authenticate: valid=%v err=%v", valid, err)
	}
	if _, err := svc.RevokeInstitutionCredential(first.ID, credential.PublicID); err != nil {
		t.Fatal(err)
	}
	if valid, _ := svc.ValidateCredential(credential.PublicID, newSecret); valid {
		t.Fatal("revoked credential should no longer authenticate")
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

func TestWebhookConfigurationPersistsAcrossRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skip("local PostgreSQL unavailable")
	}

	service, err := NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	institution, err := service.CreateInstitution("Webhook Restart Test", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	entitlements := []string{"trades:read", "quotes:write", "trades:read"}
	if err := service.SetInstitutionEntitlements(institution.ID, entitlements); err != nil {
		t.Fatal(err)
	}
	cfg, err := service.SetWebhookConfiguration(institution.ID, "https://example.test/webhooks/customer", "wh-secret-123", true, "ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookURL == "" || !cfg.Enabled {
		t.Fatal("webhook config should store endpoint and active status")
	}

	restarted, err := NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	reloadedInstitution, ok := restarted.GetInstitution(institution.ID)
	if !ok {
		t.Fatal("institution should survive restart")
	}
	if got, want := strings.Join(reloadedInstitution.Entitlements, ","), "quotes:write,trades:read"; got != want {
		t.Fatalf("entitlements mismatch after restart: got %q want %q", got, want)
	}
	loaded, ok := restarted.GetWebhookConfiguration(institution.ID)
	if !ok {
		t.Fatal("webhook config should survive restart")
	}
	if loaded.WebhookURL != cfg.WebhookURL {
		t.Fatalf("webhook url mismatch after restart: got %q want %q", loaded.WebhookURL, cfg.WebhookURL)
	}
	if loaded.Status != cfg.Status || !loaded.Enabled {
		t.Fatalf("webhook status mismatch after restart: got %+v want %+v", loaded, cfg)
	}
}

func TestCredentialLifecyclePersistsAcrossRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skip("local PostgreSQL unavailable")
	}

	service, err := NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	institution, err := service.CreateInstitution("Credential Restart Test", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	credential, initialSecret, err := service.CreateCredential(institution.ID, "production-integration", "production")
	if err != nil {
		t.Fatal(err)
	}
	_, rotatedSecret, err := service.RotateCredential(institution.ID, credential.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := restarted.ValidateCredential(credential.PublicID, rotatedSecret); err != nil || !valid {
		t.Fatalf("rotated credential did not survive restart: valid=%v err=%v", valid, err)
	}
	if valid, _ := restarted.ValidateCredential(credential.PublicID, initialSecret); valid {
		t.Fatal("pre-rotation secret remained valid after restart")
	}
	loaded, err := restarted.GetCredential(institution.ID, credential.PublicID)
	if err != nil || loaded.Environment != "production" || loaded.Label != "production-integration" || loaded.SecretHash != "" {
		t.Fatalf("persisted credential metadata invalid: credential=%+v err=%v", loaded, err)
	}
	if _, err := restarted.RevokeInstitutionCredential(institution.ID, credential.PublicID); err != nil {
		t.Fatal(err)
	}
	restarted, err = NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	if valid, _ := restarted.ValidateCredential(credential.PublicID, rotatedSecret); valid {
		t.Fatal("revoked credential remained valid after restart")
	}
}
