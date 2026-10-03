package reconciliation

import (
	"errors"
	"strings"
	"time"

	"github.com/osai/osai/pkg/provider"
)

// ReconcileFlutterwaveWindow compares a provider statement with internal
// settlement expectations. It ingests evidence and breaks only; Ledger remains
// the sole owner of financial journals.
func (s *Service) ReconcileFlutterwaveWindow(rail provider.SettlementRail, window any, watermark time.Time, expected []ExpectedTransaction) ([]*ReconciliationBreak, error) {
	if s == nil || rail == nil || watermark.IsZero() {
		return nil, errors.New("reconciliation inputs required")
	}
	transactions, err := rail.ListTransactions(window)
	if err != nil {
		return nil, err
	}
	byRef := make(map[string]StatementRow)
	duplicateRefs := make(map[string]bool)
	seenID := make(map[string]struct{})
	breaks := make([]*ReconciliationBreak, 0)
	for _, transaction := range transactions {
		if transaction.ProviderRef == "" || transaction.ClientRef == "" || transaction.Evidence.RawPayloadHash == "" {
			return nil, errors.New("provider statement evidence incomplete")
		}
		if _, seen := seenID[transaction.ProviderRef]; seen {
			continue
		}
		seenID[transaction.ProviderRef] = struct{}{}
		row := StatementRow{ID: "flw_statement_" + transaction.ProviderRef + "_" + strings.TrimPrefix(transaction.Evidence.RawPayloadHash, "sha256:"), Provider: "flutterwave", Source: "provider_statement", ProviderRef: transaction.ProviderRef, ClientRef: transaction.ClientRef, AmountMinor: transaction.AmountMinor, Currency: transaction.Currency, Beneficiary: transaction.Beneficiary, FeeMinor: transaction.FeeMinor, Account: "flutterwave", RawHash: transaction.Evidence.RawPayloadHash, Watermark: transaction.OccurredAt.UTC().Format(time.RFC3339Nano)}
		if _, err = s.IngestProviderEvidence(transaction.Evidence, row); err != nil {
			return nil, err
		}
		if prior, exists := byRef[row.ClientRef]; exists {
			duplicateRefs[row.ClientRef] = true
			breaks = append(breaks, s.upsertTransactionBreak(ExpectedTransaction{ID: row.ClientRef, Provider: "flutterwave"}, BreakTypeDuplicateExternal, "flutterwave", row.Currency, prior.AmountMinor, row.AmountMinor, prior.AmountMinor-row.AmountMinor, row.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"multiple provider IDs for one client reference"}, "manual_review"))
			continue
		}
		byRef[row.ClientRef] = row
	}
	for _, item := range expected {
		if item.ClientRef == "" {
			return nil, errors.New("expected settlement client reference required")
		}
		if item.Provider == "" {
			item.Provider = "flutterwave"
		}
		row := byRef[item.ClientRef]
		if brk := s.ReconcileTransaction(item, row); brk != nil {
			breaks = append(breaks, brk)
		}
		if !duplicateRefs[item.ClientRef] {
			id := transactionBreakID(ExpectedTransaction{ID: item.ClientRef, Provider: "flutterwave"}, BreakTypeDuplicateExternal)
			s.mu.Lock()
			brk := s.breaks[id]
			open := brk != nil && brk.State != BreakStateResolved
			s.mu.Unlock()
			if open {
				_ = s.ResolveBreak(id, "reconciliation", "duplicate provider evidence no longer present", "")
			}
		}
		delete(byRef, item.ClientRef)
	}
	for _, row := range byRef {
		breaks = append(breaks, s.upsertTransactionBreak(ExpectedTransaction{ID: row.ClientRef, Provider: "flutterwave"}, BreakTypeUnmatchedExternal, "flutterwave", row.Currency, 0, row.AmountMinor, -row.AmountMinor, row.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"provider transfer has no internal settlement expectation"}, "manual_review"))
	}
	if err := s.IngestBalanceEvidence("flutterwave", "transfers", "NGN", 0, time.Now().UTC(), watermark.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	return breaks, nil
}
