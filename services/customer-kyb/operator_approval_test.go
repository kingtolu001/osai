package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestDurableOperatorBeneficiaryApproval(t *testing.T) {
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
	institution, err := service.CreateInstitution("Operator approval test", "NG", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	institution.KYBStatus = KybStatusApproved
	service.institutions[institution.ID] = &institution
	if err = service.saveInstitution(institution); err != nil {
		t.Fatal(err)
	}
	beneficiary, err := service.RegisterBeneficiary(institution.ID, "Test recipient", "044", "0690000040")
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.CreateInstitution("Other operator scope", "NG", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	makerToken, checkerToken := "local-test-maker-token-with-32-bytes", "local-test-checker-token-with-32-bytes"
	operators := []beneficiaryOperator{
		{ActorID: "maker-1", Role: "maker", Institutions: []string{institution.ID}, tokenHash: sha256.Sum256([]byte(makerToken))},
		{ActorID: "checker-1", Role: "checker", Institutions: []string{institution.ID}, tokenHash: sha256.Sum256([]byte(checkerToken))},
	}
	handler := service.beneficiaryOperatorHandler(operators)
	body := []byte(`{"institution_id":"` + institution.ID + `","beneficiary_id":"` + beneficiary.ID + `","reason":"verified recipient"}`)
	call := func(path, token string, data []byte) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Code
	}
	if got := call("/v1/operator/beneficiaries/approve", checkerToken, body); got != http.StatusConflict {
		t.Fatalf("approval without maker = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/propose", "", body); got != http.StatusUnauthorized {
		t.Fatalf("anonymous proposal = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/propose", checkerToken, body); got != http.StatusUnauthorized {
		t.Fatalf("wrong role proposal = %d", got)
	}
	foreign := []byte(`{"institution_id":"` + other.ID + `","beneficiary_id":"` + beneficiary.ID + `","reason":"wrong scope"}`)
	if got := call("/v1/operator/beneficiaries/propose", makerToken, foreign); got != http.StatusForbidden {
		t.Fatalf("cross institution proposal = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/propose", makerToken, body); got != http.StatusNoContent {
		t.Fatalf("maker proposal = %d", got)
	}
	if _, err := service.GetApprovedBeneficiary(institution.ID, beneficiary.ID); err == nil {
		t.Fatal("proposal became executable")
	}
	if got := call("/v1/operator/beneficiaries/approve", makerToken, body); got != http.StatusUnauthorized {
		t.Fatalf("maker self approval = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/approve", checkerToken, body); got != http.StatusNoContent {
		t.Fatalf("checker approval = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/approve", checkerToken, body); got != http.StatusConflict {
		t.Fatalf("approval replay = %d", got)
	}
	if _, err := service.GetApprovedBeneficiary(institution.ID, beneficiary.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.GetApprovedBeneficiary(institution.ID, beneficiary.ID); err != nil {
		t.Fatalf("approval lost across restart: %v", err)
	}
	var actions int
	if err := db.QueryRow(`SELECT count(*) FROM customer_beneficiary_actions WHERE beneficiary_id=$1`, beneficiary.ID).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("audit actions=%d err=%v", actions, err)
	}
	if _, err := db.Exec(`UPDATE customer_beneficiary_actions SET reason='changed' WHERE beneficiary_id=$1`, beneficiary.ID); err == nil {
		t.Fatal("audit action was mutable")
	}
	if got := call("/v1/operator/beneficiaries/disable", checkerToken, body); got != http.StatusNoContent {
		t.Fatalf("operator disable = %d", got)
	}
	restarted, err = NewPostgresService(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.GetApprovedBeneficiary(institution.ID, beneficiary.ID); err == nil {
		t.Fatal("disabled beneficiary executable")
	}
	rejected, err := service.RegisterBeneficiary(institution.ID, "Rejected recipient", "044", "0690000041")
	if err != nil {
		t.Fatal(err)
	}
	rejectBody := []byte(`{"institution_id":"` + institution.ID + `","beneficiary_id":"` + rejected.ID + `","reason":"failed review"}`)
	if got := call("/v1/operator/beneficiaries/propose", makerToken, rejectBody); got != http.StatusNoContent {
		t.Fatalf("rejection proposal = %d", got)
	}
	if got := call("/v1/operator/beneficiaries/reject", checkerToken, rejectBody); got != http.StatusNoContent {
		t.Fatalf("operator rejection = %d", got)
	}
	if _, err := service.GetApprovedBeneficiary(institution.ID, rejected.ID); err == nil {
		t.Fatal("rejected beneficiary executable")
	}
}
