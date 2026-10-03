package main

import "testing"

func TestApprovedBeneficiaryIsInstitutionScopedAndCanBeDisabled(t *testing.T) {
	service := NewService()
	first, err := service.CreateInstitution("First", "NG", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateInstitution("Second", "NG", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	service.institutions[first.ID].KYBStatus = KybStatusApproved
	service.institutions[second.ID].KYBStatus = KybStatusApproved
	beneficiary, err := service.RegisterBeneficiary(first.ID, "Institution account", "044", "0690000040")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetApprovedBeneficiary(first.ID, beneficiary.ID); err == nil {
		t.Fatal("pending account was executable")
	}
	// This in-memory fixture exercises access checks; production approval uses
	// the authenticated, durable operator path covered by its integration test.
	service.beneficiaries[beneficiary.ID].ApprovalStatus = BeneficiaryApprovalApproved
	if _, err := service.GetApprovedBeneficiary(second.ID, beneficiary.ID); err == nil {
		t.Fatal("cross-institution account was accessible")
	}
	if got, err := service.GetApprovedBeneficiary(first.ID, beneficiary.ID); err != nil || got.AccountNumber != "0690000040" {
		t.Fatalf("approved account unavailable: %+v %v", got, err)
	}
	if err := service.DisableBeneficiary(first.ID, beneficiary.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetApprovedBeneficiary(first.ID, beneficiary.ID); err == nil {
		t.Fatal("disabled account was executable")
	}
}

func TestBeneficiaryRegistrationReplayUsesStableIdentity(t *testing.T) {
	service := NewService()
	institution, err := service.CreateInstitution("Institution", "NG", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	service.institutions[institution.ID].KYBStatus = KybStatusApproved
	first, err := service.RegisterBeneficiary(institution.ID, "Account", "044", "0690000040", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.RegisterBeneficiary(institution.ID, "Account", "044", "0690000040", "request-1")
	if err != nil || again.ID != first.ID {
		t.Fatalf("registration replay changed record: %+v %v", again, err)
	}
	if _, err := service.RegisterBeneficiary(institution.ID, "Different", "044", "0690000040", "request-1"); err == nil {
		t.Fatal("changed details reused idempotency key")
	}
}
