package main

import (
    "testing"
)

func TestSeedRuntimeProbe(t *testing.T) {
    t.Setenv("OSAI_SANDBOX_INSTITUTION_ID", "inst_sandbox_local")
    t.Setenv("OSAI_SANDBOX_CLIENT_ID", "ck_sandbox_local")
    t.Setenv("OSAI_SANDBOX_CLIENT_SECRET", "secret_local_001")

    cs := NewCustomerService()
    seedInstitution(cs)

    id, err := cs.InstitutionForAPIKey("ck_sandbox_local", "secret_local_001")
    if err != nil {
        t.Fatalf("expected seeded credential to authenticate: %v", err)
    }
    if id != "inst_sandbox_local" {
        t.Fatalf("expected institution inst_sandbox_local, got %s", id)
    }
    if _, ok := cs.institutions["inst_sandbox_local"]; !ok {
        t.Fatal("institution missing from in-memory seed")
    }
    if _, ok := cs.publicToCred["ck_sandbox_local"]; !ok {
        t.Fatal("credential missing from in-memory seed")
    }
    if got, want := cs.publicToCred["ck_sandbox_local"].SecretHash, hashCustomerSecret("secret_local_001", "inst_sandbox_local"); got != want {
        t.Fatalf("secret hashing mismatch: got %s want %s", got, want)
    }
}
