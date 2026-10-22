package ledgercore

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"
)

// These are executable accounting examples, not a posting service or approval.
// Fixture accounts exist only in test builds and cannot select a runtime route.
func TestSandboxAccountingPolicyV1(t *testing.T) {
	raw, err := os.ReadFile("testdata/sandbox-accounting-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Version     string   `json:"version"`
		Environment string   `json:"environment"`
		Roles       []string `json:"roles"`
		Scenarios   []struct {
			Name     string `json:"name"`
			Journals []struct {
				Event    string `json:"event"`
				Currency string `json:"currency"`
				Entries  []struct {
					Role   string `json:"role"`
					Debit  string `json:"debit_minor"`
					Credit string `json:"credit_minor"`
				} `json:"entries"`
			} `json:"journals"`
			Expected map[string]string `json:"expected"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Version != "osai-sandbox-accounting/v1" || policy.Environment != "test-only" || len(policy.Scenarios) != 6 {
		t.Fatal("unexpected policy scope or missing scenarios")
	}
	roles := map[string]bool{}
	for _, role := range policy.Roles {
		roles[role] = true
	}
	amount := func(value string) int64 {
		t.Helper()
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("invalid exact minor units %q: %v", value, err)
		}
		return n
	}
	for _, scenario := range policy.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			balances := map[string]int64{}
			for index, vector := range scenario.Journals {
				journal := NewJournal("sbx:"+scenario.Name+":"+strconv.Itoa(index), "sbx:trade", vector.Currency)
				journal.InstitutionID = "sbx:institution"
				for entryIndex, entry := range vector.Entries {
					if !roles[entry.Role] || (vector.Currency != "NGN" && vector.Currency != "USD") {
						t.Fatal("unknown account role or currency")
					}
					debit, credit := amount(entry.Debit), amount(entry.Credit)
					if debit < 0 || credit < 0 || (debit == 0 && credit == 0) {
						t.Fatal("invalid posting amount")
					}
					owner := "sbx:institution"
					if entry.Role == "custody" {
						owner = "sbx:custodian"
					}
					if entry.Role == "fee_revenue" || entry.Role == "fee_refunds" || entry.Role == "provider_fee_expense" || entry.Role == "provider_fee_payable" {
						owner = "sbx:osai"
					}
					journal.AddEntry("sbx:"+vector.Currency+":"+entry.Role, entry.Role, owner, vector.Currency, debit, credit, journal.ID+":"+strconv.Itoa(entryIndex))
					balances[vector.Currency+":"+entry.Role] += debit - credit
				}
				if err := journal.Validate(); err != nil {
					t.Fatalf("%s: %v", vector.Event, err)
				}
			}
			for key, value := range balances {
				if value == 0 {
					delete(balances, key)
				}
			}
			expected := map[string]int64{}
			for key, value := range scenario.Expected {
				expected[key] = amount(value)
			}
			if !reflect.DeepEqual(balances, expected) {
				t.Fatalf("economic outcome mismatch: got %v want %v", balances, expected)
			}
		})
	}
}

func TestSandboxReservationAndCurrencyIsolation(t *testing.T) {
	account := &LedgerAccount{Code: "sbx:NGN:available", Owner: "sbx:institution", Currency: "NGN", Available: 10200}
	if err := PlaceHold(account, 10200); err != nil {
		t.Fatal(err)
	}
	if account.Available != 0 || account.Held != 10200 {
		t.Fatal("all-in debit was not reserved")
	}
	if err := PlaceHold(account, 1); err == nil {
		t.Fatal("overspending accepted")
	}
	if err := ReleaseHold(account, 10200); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseHold(account, 1); err == nil {
		t.Fatal("duplicate release accepted")
	}
	if account.Available != 10200 || account.Held != 0 {
		t.Fatal("release changed total customer entitlement")
	}
	journal := NewJournal("sbx:invalid", "sbx:trade", "NGN")
	journal.AddEntry("sbx:NGN:held", "held", "sbx:institution", "NGN", 100, 0, "sbx:debit")
	journal.AddEntry("sbx:USD:custody", "custody", "sbx:custodian", "USD", 0, 100, "sbx:credit")
	if err := journal.Validate(); err == nil {
		t.Fatal("cross-currency balancing plug accepted")
	}
}
