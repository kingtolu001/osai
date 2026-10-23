package reconciliation

import (
	"encoding/hex"
	"errors"
	"github.com/osai/osai/pkg/sandboxrecon"
	"math"
	"strings"
	"time"
)

// ReconcileSandboxSnapshot compares an owner-authenticated Ledger snapshot with
// an independently signed fixture statement. It records evidence and breaks;
// there is no financial writer or balancing-entry interface in this path.
func (s *Service) ReconcileSandboxSnapshot(ledger sandboxrecon.Snapshot, statement sandboxrecon.Statement, hash string) ([]*ReconciliationBreak, error) {
	if s == nil || s.store == nil || !strings.HasPrefix(ledger.InstitutionID, "sbx:") || ledger.TradeID == "" || ledger.InstitutionID != statement.Snapshot.InstitutionID || ledger.TradeID != statement.Snapshot.TradeID || len(hash) != 64 || statement.ObservedAt.IsZero() || len(ledger.Movements) > 1000 || len(statement.Snapshot.Movements) > 1000 {
		return nil, errors.New("bounded authenticated sandbox reconciliation inputs required")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil, errors.New("invalid statement evidence hash")
	}
	for _, snapshot := range []sandboxrecon.Snapshot{ledger, statement.Snapshot} {
		if len(snapshot.ClosingMinor) != 2 {
			return nil, errors.New("unsupported closing currency")
		}
		for _, currency := range []string{"NGN", "USD"} {
			value, ok := snapshot.ClosingMinor[currency]
			if !ok || value > math.MaxInt64/4 || value < -math.MaxInt64/4 {
				return nil, errors.New("complete exact sandbox closing balances required")
			}
		}
		for _, m := range snapshot.Movements {
			if m.Reference == "" || m.Kind == "" || (m.Currency != "NGN" && m.Currency != "USD") || m.DeltaMinor > math.MaxInt64/4 || m.DeltaMinor < -math.MaxInt64/4 {
				return nil, errors.New("unsupported sandbox statement movement")
			}
		}
	}
	if err := s.store.SaveEvidence(Evidence{ID: "sbx:statement:" + hash, Source: "independent_signed_sandbox_statement", Provider: "sbx:provider", Account: ledger.InstitutionID, RawPayloadHash: hash, IngestedAt: time.Now().UTC(), Watermark: statement.ObservedAt.Format(time.RFC3339Nano), CorrelationID: ledger.TradeID, RawPayload: statement}); err != nil {
		return nil, err
	}
	key := func(m sandboxrecon.Movement) string {
		return ledger.InstitutionID + ":" + ledger.TradeID + ":" + m.Reference + ":" + m.Kind + ":" + m.Currency
	}
	observed := map[string]sandboxrecon.Movement{}
	duplicates := map[string]bool{}
	var breaks []*ReconciliationBreak
	for _, m := range statement.Snapshot.Movements {
		k := key(m)
		if _, ok := observed[k]; ok {
			duplicates[k] = true
		}
		observed[k] = m
	}
	expected := map[string]bool{}
	for _, m := range ledger.Movements {
		k := key(m)
		if expected[k] {
			return nil, errors.New("duplicate owner movement identity")
		}
		expected[k] = true
		want := ExpectedTransaction{ID: k, Provider: "sbx:provider", ClientRef: k, ProviderRef: m.Reference, AmountMinor: m.DeltaMinor, Currency: m.Currency, Account: ledger.InstitutionID}
		var brk *ReconciliationBreak
		var err error
		if duplicates[k] {
			brk, err = s.upsertTransactionBreak(want, BreakTypeDuplicateExternal, "sbx:provider", m.Currency, m.DeltaMinor, observed[k].DeltaMinor, 0, observed[k].DeltaMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"duplicate signed fixture movement"}, "manual_review")
		} else {
			var row StatementRow
			if got, ok := observed[k]; ok {
				row = StatementRow{ID: "sbx:row:" + hash + ":" + k, Provider: "sbx:provider", Source: "signed_fixture_statement", ClientRef: k, ProviderRef: got.Reference, AmountMinor: got.DeltaMinor, Currency: got.Currency, Account: ledger.InstitutionID, RawHash: hash}
			}
			brk, err = s.ReconcileTransaction(want, row)
		}
		if err != nil {
			return nil, err
		}
		if brk != nil {
			breaks = append(breaks, brk)
		}
		delete(observed, k)
	}
	for k, m := range observed {
		b, err := s.upsertTransactionBreak(ExpectedTransaction{ID: k, Provider: "sbx:provider"}, BreakTypeUnmatchedExternal, "sbx:provider", m.Currency, 0, m.DeltaMinor, -m.DeltaMinor, m.DeltaMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"statement movement absent from owner journals"}, "manual_review")
		if err != nil {
			return nil, err
		}
		breaks = append(breaks, b)
	}
	for _, currency := range []string{"NGN", "USD"} {
		want, got := ledger.ClosingMinor[currency], statement.Snapshot.ClosingMinor[currency]
		id := ExpectedTransaction{ID: ledger.InstitutionID + ":closing:" + currency, Provider: "sbx:provider"}
		if want != got {
			b, err := s.upsertTransactionBreak(id, BreakTypeBalanceMismatch, "sbx:provider", currency, want, got, want-got, got, 0, "reconciliation", BreakStateOpen, nil, []string{"independent fixture custody closing mismatch"}, "manual_review")
			if err != nil {
				return nil, err
			}
			breaks = append(breaks, b)
		}
	}

	for _, brk := range breaks {
		brk.CorrelationID = ledger.TradeID
		brk.EvidenceIDs = append(brk.EvidenceIDs, "sbx:statement:"+hash)
		if err := s.store.SaveBreak(brk); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.breaks[brk.ID] = cloneBreak(brk)
		s.mu.Unlock()
	}
	// An old unresolved duplicate or closing break must not disappear merely
	// because the latest scan produced no new discrepancies.
	all, err := s.store.LoadBreaks()
	if err != nil {
		return nil, err
	}
	var open []*ReconciliationBreak
	for _, brk := range all {
		if brk.CorrelationID == ledger.TradeID && brk.State != BreakStateResolved {
			open = append(open, brk)
		}
	}
	return open, nil
}
