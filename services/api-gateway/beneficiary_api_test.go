package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type beneficiaryCustomerClientStub struct {
	stubCustomerClient
	registrations int
}

func (beneficiaryCustomerClientStub) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", APIEnabled: true,
		Entitlements: []string{"quotes:read", "quotes:write", "beneficiaries:write"},
	}, nil
}

func (c *beneficiaryCustomerClientStub) RegisterBeneficiary(_ context.Context, institutionID, _, _, _, _ string) (map[string]any, error) {
	c.registrations++
	return map[string]any{
		"beneficiary_id":  "ben_123",
		"institution_id":  institutionID,
		"status":          "ACTIVE",
		"approval_status": "PENDING",
	}, nil
}

type blockingBeneficiaryCustomerClient struct {
	stubCustomerClient
	mu            sync.Mutex
	registrations int
	started       chan struct{}
	release       chan struct{}
}

func (*blockingBeneficiaryCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", APIEnabled: true,
		Entitlements: []string{"quotes:read", "quotes:write", "beneficiaries:write"},
	}, nil
}

func (c *blockingBeneficiaryCustomerClient) RegisterBeneficiary(_ context.Context, institutionID, _, _, _, _ string) (map[string]any, error) {
	c.mu.Lock()
	c.registrations++
	if c.registrations == 1 {
		close(c.started)
	}
	c.mu.Unlock()
	<-c.release
	return map[string]any{"beneficiary_id": "ben_concurrent", "institution_id": institutionID, "status": "ACTIVE", "approval_status": "PENDING"}, nil
}

func TestRegisterBeneficiaryIsIdempotentAndInstitutionScoped(t *testing.T) {
	customer := &beneficiaryCustomerClientStub{}
	gateway := NewGatewayWithClients(customer, stubQuoteClient{})
	gateway.idempotency = &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	server := httptest.NewServer(gateway)
	defer server.Close()

	body := `{"name":"Recipient","bank_code":"044","account_number":"0690000040"}`
	first := doJSON(t, server, http.MethodPost, "/v1/beneficiaries", "ApiKey ck_ok:s3cr3t", "beneficiary-1", body)
	if first.StatusCode != http.StatusAccepted || first.Body["beneficiary_id"] != "ben_123" || first.Body["approval_status"] != "PENDING" {
		t.Fatalf("expected accepted pending beneficiary, got %d: %v", first.StatusCode, first.Body)
	}
	replay := doJSON(t, server, http.MethodPost, "/v1/beneficiaries", "ApiKey ck_ok:s3cr3t", "beneficiary-1", body)
	if replay.StatusCode != http.StatusAccepted || replay.Body["beneficiary_id"] != "ben_123" {
		t.Fatalf("expected original registration response on replay, got %d: %v", replay.StatusCode, replay.Body)
	}
	if customer.registrations != 1 {
		t.Fatalf("expected one customer-service registration, got %d", customer.registrations)
	}

	conflict := doJSON(t, server, http.MethodPost, "/v1/beneficiaries", "ApiKey ck_ok:s3cr3t", "beneficiary-1", strings.Replace(body, "Recipient", "Other", 1))
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("expected idempotency conflict, got %d: %v", conflict.StatusCode, conflict.Body)
	}
	if customer.registrations != 1 {
		t.Fatalf("conflicting replay must not call customer service, got %d calls", customer.registrations)
	}
}

func TestConcurrentMutationsShareOneIdempotencyReservation(t *testing.T) {
	customer := &blockingBeneficiaryCustomerClient{started: make(chan struct{}), release: make(chan struct{})}
	gateway := NewGatewayWithClients(customer, stubQuoteClient{})
	gateway.idempotency = &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	server := httptest.NewServer(gateway)
	defer server.Close()
	body := `{"name":"Recipient","bank_code":"044","account_number":"0690000040"}`
	type result struct {
		status int
		body   map[string]any
		err    error
	}
	send := func() result {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/beneficiaries", strings.NewReader(body))
		if err != nil {
			return result{err: err}
		}
		request.Header.Set("Authorization", "ApiKey ck_ok:s3cr3t")
		request.Header.Set("Idempotency-Key", "same-concurrent-key")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return result{err: err}
		}
		defer response.Body.Close()
		var responseBody map[string]any
		err = json.NewDecoder(response.Body).Decode(&responseBody)
		return result{status: response.StatusCode, body: responseBody, err: err}
	}
	firstResult := make(chan result, 1)
	go func() { firstResult <- send() }()
	<-customer.started
	concurrent := send()
	if concurrent.err != nil || concurrent.status != http.StatusConflict || concurrent.body["error"] == nil {
		t.Fatalf("concurrent duplicate should report in progress: %+v", concurrent)
	}
	close(customer.release)
	first := <-firstResult
	if first.err != nil || first.status != http.StatusAccepted {
		t.Fatalf("first mutation failed: %+v", first)
	}
	replay := send()
	if replay.err != nil || replay.status != first.status {
		t.Fatalf("completed mutation did not replay status: %+v", replay)
	}
	if replay.body["request_id"] != first.body["request_id"] || replay.body["beneficiary_id"] != first.body["beneficiary_id"] {
		t.Fatalf("replay did not return original response: first=%v replay=%v", first.body, replay.body)
	}
	customer.mu.Lock()
	defer customer.mu.Unlock()
	if customer.registrations != 1 {
		t.Fatalf("expected one downstream registration, got %d", customer.registrations)
	}
}

func TestGatewayMutationFailsClosedWhenIdempotencyDatabaseIsUnavailable(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://localhost/unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	customer := &beneficiaryCustomerClientStub{}
	gateway := NewGatewayWithClients(customer, stubQuoteClient{})
	gateway.idempotency = &idempotencyStore{db: db, rows: map[string]*idempotencyEntry{}}
	server := httptest.NewServer(gateway)
	defer server.Close()
	response := doJSON(t, server, http.MethodPost, "/v1/beneficiaries", "ApiKey ck_ok:s3cr3t", "db-down-key", `{"name":"Recipient","bank_code":"044","account_number":"0690000040"}`)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("database failure should fail closed with 503, got %d: %v", response.StatusCode, response.Body)
	}
	if customer.registrations != 0 {
		t.Fatalf("mutation reached customer service despite idempotency database outage: %d calls", customer.registrations)
	}
}
