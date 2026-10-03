package reconciliation

import (
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
)

type statementRail struct {
	items []provider.ExternalTransaction
}

func (r statementRail) CreateTransfer(provider.TransferInstruction) (provider.TransferAck, error) {
	return provider.TransferAck{}, nil
}
func (r statementRail) GetTransfer(string) (provider.TransferResult, error) {
	return provider.TransferResult{}, nil
}
func (r statementRail) VerifyWebhook([]byte, map[string]string) (provider.WebhookEvent, error) {
	return provider.WebhookEvent{}, nil
}
func (r statementRail) ListTransactions(any) ([]provider.ExternalTransaction, error) {
	return r.items, nil
}
func (r statementRail) Health() provider.Health { return provider.Health{Up: true} }

func TestFlutterwaveStatementComparison(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	base := provider.ExternalTransaction{ID: "42", ProviderRef: "42", ClientRef: "si_1", AmountMinor: 10000, Currency: "NGN", Beneficiary: "0690000040", OccurredAt: now, Evidence: provider.Evidence{ID: "e42", Provider: "flutterwave", RequestRef: "si_1", RawPayloadHash: "sha256:one"}}
	want := ExpectedTransaction{ID: "si_1", Provider: "flutterwave", ProviderRef: "42", ClientRef: "si_1", AmountMinor: 10000, Currency: "NGN", Beneficiary: "0690000040"}
	for _, tc := range []struct {
		name      string
		items     []provider.ExternalTransaction
		expected  ExpectedTransaction
		breakType BreakType
	}{
		{"clean", []provider.ExternalTransaction{base}, want, ""},
		{"missing", nil, want, BreakTypeMissingExternalEvidence},
		{"amount", []provider.ExternalTransaction{func() provider.ExternalTransaction { x := base; x.AmountMinor = 9000; return x }()}, want, BreakTypeAmountMismatch},
		{"currency", []provider.ExternalTransaction{func() provider.ExternalTransaction { x := base; x.Currency = "USD"; return x }()}, want, BreakTypeCurrencyMismatch},
		{"late", []provider.ExternalTransaction{base}, func() ExpectedTransaction { x := want; x.Watermark = now.Add(time.Hour).Format(time.RFC3339); return x }(), BreakTypeLateEvidence},
		{"duplicate provider IDs", []provider.ExternalTransaction{base, func() provider.ExternalTransaction {
			x := base
			x.ID = "43"
			x.ProviderRef = "43"
			x.Evidence.ID = "e43"
			x.Evidence.RawPayloadHash = "sha256:two"
			return x
		}()}, want, BreakTypeDuplicateExternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService()
			breaks, err := service.ReconcileFlutterwaveWindow(statementRail{items: tc.items}, struct{}{}, now.Add(time.Hour), []ExpectedTransaction{tc.expected})
			if err != nil {
				t.Fatal(err)
			}
			if tc.breakType == "" && len(breaks) != 0 {
				t.Fatalf("unexpected breaks: %+v", breaks)
			}
			if tc.breakType != "" {
				found := false
				for _, brk := range breaks {
					if brk.Type == tc.breakType {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing %s: %+v", tc.breakType, breaks)
				}
			}
		})
	}
}
