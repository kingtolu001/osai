package integration

import (
	"context"
	"encoding/json"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"net/http"
	"strings"
	"testing"
)

// These authenticated fixture operators approve test beneficiaries only.
func startApprovedSandboxCustomer(t *testing.T, r *proofRuntime) (string, string) {
	t.Helper()
	ctx := context.Background()
	address, httpAddress := loopbackPort(t), loopbackPort(t)
	maker, checker, writeToken := randomHex(t, 32), randomHex(t, 32), randomHex(t, 32)
	operators, _ := json.Marshal([]map[string]any{{"actor_id": "sbx:customer-maker", "token": maker, "role": "maker", "institutions": []string{"sbx:institution"}}, {"actor_id": "sbx:customer-checker", "token": checker, "role": "checker", "institutions": []string{"sbx:institution"}}})
	for k, v := range map[string]string{"OSAI_CUSTOMER_GRPC_ADDR": address, "OSAI_CUSTOMER_POSTGRES_DSN": r.env["OSAI_POSTGRES_DSN"], "OSAI_KYB_PORT": httpAddress, "OSAI_SANDBOX_SEED": "1", "OSAI_SANDBOX_INSTITUTION_ID": "sbx:institution", "OSAI_SANDBOX_CLIENT_ID": "sbx:actual-customer", "OSAI_SANDBOX_CLIENT_SECRET": randomHex(t, 32), "OSAI_BENEFICIARY_WRITE_TOKEN": writeToken, "OSAI_BENEFICIARY_OPERATORS_JSON": string(operators)} {
		r.env[k] = v
	}
	r.start("actual-customer", "./services/customer-kyb", address)
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	customer := customerv1.NewCustomerServiceClient(conn)
	beneficiary, err := customer.RegisterBeneficiary(metadata.AppendToOutgoingContext(ctx, "x-osai-beneficiary-write-token", writeToken), &customerv1.RegisterBeneficiaryRequest{InstitutionId: "sbx:institution", Name: "Explicit sandbox fixture recipient", BankCode: "044", AccountNumber: "0690000040", IdempotencyKey: "sbx:actual-approved-beneficiary"})
	if err != nil || beneficiary == nil || !strings.HasPrefix(beneficiary.BeneficiaryId, "ben_") {
		t.Fatal("actual Customer beneficiary registration failed", err)
	}
	request := &customerv1.BeneficiaryRequest{InstitutionId: "sbx:institution", BeneficiaryId: beneficiary.BeneficiaryId}
	if _, err = customer.GetApprovedBeneficiary(ctx, request); err == nil {
		t.Fatal("unapproved actual Customer beneficiary admitted")
	}
	body, _ := json.Marshal(map[string]string{"institution_id": "sbx:institution", "beneficiary_id": beneficiary.BeneficiaryId, "reason": "explicit test-only beneficiary approval"})
	for _, action := range []struct{ name, token string }{{"propose", maker}, {"approve", checker}} {
		code, _ := proofHTTP(t, http.MethodPost, "http://"+httpAddress+"/v1/operator/beneficiaries/"+action.name, "Bearer "+action.token, "", "", string(body))
		if code != http.StatusNoContent {
			t.Fatalf("actual Customer %s returned %d", action.name, code)
		}
	}
	r.stop("actual-customer")
	r.start("actual-customer", "./services/customer-kyb", address)
	approved, err := customer.GetApprovedBeneficiary(ctx, request)
	if err != nil || approved.BeneficiaryId != beneficiary.BeneficiaryId || approved.ApprovalStatus != "APPROVED" {
		t.Fatal("actual Customer approval did not survive restart", err)
	}
	t.Log("ACTUAL_CUSTOMER registered ben_UUID, rejected pending beneficiary, applied separate authenticated maker/checker approval and recovered approved identity after restart")
	return address, beneficiary.BeneficiaryId
}
