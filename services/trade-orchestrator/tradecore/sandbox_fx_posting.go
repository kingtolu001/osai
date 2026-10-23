package tradecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/osai/osai/pkg/fxevidence"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
)

// SandboxFXTerms exposes accepted economics before execution. Reserving these
// terms must not depend on future execution/custody evidence.
func (s *Store) SandboxFXTerms(tradeID, executor string) (sandboxaccounting.Terms, error) {
	trade, ok := s.GetTrade(tradeID)
	if !ok || trade.PricingSnapshot == nil || !strings.HasPrefix(trade.InstitutionID, "sbx:") || !sandboxaccounting.FixtureQuoteID(trade.QuoteID) || !strings.HasPrefix(executor, "sbx:") || !sandboxaccounting.ValidBeneficiaryID(trade.BeneficiaryID) || trade.SettlementProviderID != "sbx:provider" {
		return sandboxaccounting.Terms{}, errors.New("explicit sandbox trade and executor required")
	}
	p := trade.PricingSnapshot
	if err := pricing.Validate(*p); err != nil {
		return sandboxaccounting.Terms{}, err
	}
	if p.SourceCurrency != "NGN" || p.DestinationCurrency != "USD" || p.SourceExponent != 2 || p.DestinationExponent != 2 {
		return sandboxaccounting.Terms{}, errors.New("sandbox FX posting supports only the NGN/USD fixture")
	}
	rate := new(big.Rat).SetFrac64(p.RateScaled, pricing.RateScale)
	if rate.Cmp(new(big.Rat).SetFrac64(p.DestinationAmountMinor, p.SourcePrincipalMinor)) != 0 {
		return sandboxaccounting.Terms{}, errors.New("sandbox FX posting requires exact conversion without remainder")
	}
	decimal := strings.TrimRight(strings.TrimRight(rate.FloatString(8), "0"), ".")
	return sandboxaccounting.Terms{TradeID: trade.ID, InstitutionID: trade.InstitutionID, PrincipalMinor: p.SourcePrincipalMinor, FeeMinor: p.FeeMinor, Plan: &sandboxaccounting.FXPlan{QuoteID: trade.QuoteID, Executor: executor, AppliedRate: decimal, DestinationMinor: p.DestinationAmountMinor, RateDirection: fxevidence.DestinationPerSource, BeneficiaryID: trade.BeneficiaryID, ProviderID: trade.SettlementProviderID}}, nil
}

func (s *Store) validateSandboxPosting(ctx context.Context) error {
	enabled, err := sandboxpolicy.Read()
	if err != nil || !enabled {
		return errors.New("sandbox FX posting requires the explicit local accounting profile")
	}
	return s.ValidateSandboxFXRecovery(ctx)
}

// EnqueueSandboxFXPosting freezes a posting command before any Ledger RPC.
// heldRevision is the revision of an already committed source reservation;
// this method never fabricates funding or releases destination payout.
func (s *Store) EnqueueSandboxFXPosting(ctx context.Context, tradeID string, heldRevision int64) error {
	if err := s.validateSandboxPosting(ctx); err != nil {
		return err
	}
	if heldRevision < 1 {
		return errors.New("committed source reservation revision required")
	}
	e, ok, err := s.GetFXExecution(tradeID)
	if err != nil {
		return err
	}
	if !ok || e.Status != FXExecutionComplete || e.ExecutionEvidence == nil || e.LastRecoveryEvidence == nil || e.LastRecoveryEvidence.ProviderRef != e.ExecutionEvidence.ExecutionReference || len(e.LastRecoveryEvidence.PayloadHash) != 64 {
		return errors.New("durably verified completed FX execution required")
	}
	proof := *e.LastRecoveryEvidence
	proof.RawPayload = e.VerifiedResponse
	if err := validateFXQueryEvidence(e, proof); err != nil {
		return errors.New("durable authenticated FX response is missing or corrupted")
	}
	terms, err := s.SandboxFXTerms(tradeID, e.ExecutorID)
	if err != nil {
		return err
	}
	if err = validateFXExecution(e, *e.ExecutionEvidence); err != nil {
		return err
	}
	if e.ExecutionEvidence.DestinationProceedsMinor != terms.Plan.DestinationMinor || e.ExecutionEvidence.AppliedRate != terms.Plan.AppliedRate || e.ExecutionEvidence.RateDirection != terms.Plan.RateDirection {
		return errors.New("FX execution differs from accepted sandbox plan")
	}
	trade, ok := s.GetTrade(tradeID)
	if !ok {
		return errors.New("sandbox trade unavailable")
	}
	payload, err := json.Marshal(e.ExecutionEvidence)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	command := sandboxaccounting.Command{ExpectedRevision: heldRevision, PolicyVersion: sandboxaccounting.Version, EventID: "sbx:ledger-fx:" + e.ID, Event: "fx_complete", Actor: e.ExecutorID, CorrelationID: trade.CorrelationID, Reason: "verified local sandbox FX execution", EvidenceHash: hex.EncodeToString(hash[:]), EvidencePayload: string(payload), Terms: terms, Execution: e.ExecutionEvidence}
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO trade_sandbox_fx_postings(event_id,execution_id,command_json) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, command.EventID, e.ID, string(raw)); err != nil {
		return err
	}
	var existing string
	if err = s.db.QueryRowContext(ctx, `SELECT command_json FROM trade_sandbox_fx_postings WHERE event_id=$1 AND execution_id=$2`, command.EventID, e.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(raw) {
		return errors.New("sandbox FX posting command conflict")
	}
	return nil
}

// RelaySandboxFXPostings retries the identical command after ambiguous RPC or
// local receipt failures. Ledger owns atomic posting and idempotent replay.
func (s *Store) RelaySandboxFXPostings(ctx context.Context, client ledgerv1.LedgerServiceClient, limit int) (int, error) {
	if client == nil || limit < 1 || limit > 100 {
		return 0, errors.New("bounded sandbox Ledger relay required")
	}
	if err := s.validateSandboxPosting(ctx); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,command_json FROM trade_sandbox_fx_postings WHERE posted_at IS NULL ORDER BY COALESCE(attempted_at,created_at),event_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type job struct{ event, raw string }
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.event, &j.raw); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	processed := 0
	var failures []error
	for _, j := range jobs {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE trade_sandbox_fx_postings SET attempted_at=now() WHERE event_id=$1 AND posted_at IS NULL`, j.event); err != nil {
			failures = append(failures, err)
			continue
		}
		receipt, callErr := client.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: j.raw})
		if callErr != nil {
			failures = append(failures, errors.New("sandbox FX Ledger outcome unresolved"))
			continue
		}
		if err := validateSandboxFXPostingReceipt(j.event, j.raw, receipt); err != nil {
			failures = append(failures, errors.New("sandbox FX Ledger receipt mismatch"))
			continue
		}
		raw, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			failures = append(failures, marshalErr)
			continue
		}
		result, updateErr := s.db.ExecContext(ctx, `UPDATE trade_sandbox_fx_postings SET posted_at=now(),receipt_json=$2 WHERE event_id=$1 AND command_json=$3 AND posted_at IS NULL`, j.event, string(raw), j.raw)
		if updateErr != nil {
			failures = append(failures, fmt.Errorf("sandbox FX Ledger receipt not persisted: %w", updateErr))
			continue
		}
		written, updateErr := result.RowsAffected()
		if updateErr != nil {
			failures = append(failures, updateErr)
			continue
		}
		processed += int(written)
	}
	return processed, errors.Join(failures...)
}

// A state name alone cannot establish which committed Ledger revision the
// receipt acknowledges. Validate against the frozen command before recording
// completion, including when Ledger returns an idempotent replay.
func validateSandboxFXPostingReceipt(event, raw string, receipt *ledgerv1.SandboxAccountingResponse) error {
	var command sandboxaccounting.Command
	if err := json.Unmarshal([]byte(raw), &command); err != nil {
		return errors.New("invalid frozen sandbox FX posting command")
	}
	if command.EventID != event || command.Event != "fx_complete" || command.ExpectedRevision < 1 || command.ExpectedRevision == int64(9223372036854775807) || receipt == nil || receipt.EventId != event || receipt.State != "FX_HELD" || receipt.Revision != command.ExpectedRevision+1 {
		return errors.New("sandbox FX Ledger receipt mismatch")
	}
	return nil
}
