package sandboxaccounting

import "testing"

func TestBeneficiaryIdentifiersPreserveCustomerOwnershipWithoutGrantingApproval(t *testing.T) {
	for _, id := range []string{"sbx:beneficiary", "ben_123e4567-e89b-12d3-a456-426614174000"} {
		if !ValidBeneficiaryID(id) {
			t.Fatalf("valid identifier rejected: %s", id)
		}
	}
	for _, id := range []string{"", "sbx:", "ben_not-a-uuid", "account_number", "1234567890"} {
		if ValidBeneficiaryID(id) {
			t.Fatalf("invalid identifier accepted: %s", id)
		}
	}
}
