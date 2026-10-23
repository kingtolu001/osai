package tradecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
)

type sandboxFXReservation struct {
	event, command, receipt string
	revision                int64
}

func (s *Store) SetSandboxFundingRequired(tradeID string) error {
	if s == nil || s.db == nil {
		return errors.New("durable sandbox funding state required")
	}
	return s.setStatusWithFundingFenceDB(tradeID, StateFundingRequired, true)
}

// ReserveSandboxFX reserves existing fixture prefund over the Ledger contract,
// then atomically records the receipt and first execution intent in Trade.
// Ledger and Trade crashes can replay the same reserve without a second hold.
func (s *Store) ReserveSandboxFX(ctx context.Context, tradeID string, client ledgerv1.LedgerServiceClient) (*FXExecution, error) {
	enabled, err := sandboxpolicy.FXAcceptance()
	if err != nil || !enabled || client == nil {
		return nil, errors.New("complete sandbox FX acceptance profile required")
	}
	if err := s.validateSandboxPosting(ctx); err != nil {
		return nil, err
	}
	executor := os.Getenv("OSAI_SANDBOX_FX_EXECUTOR_ID")
	terms, err := s.SandboxFXTerms(tradeID, executor)
	if err != nil {
		return nil, err
	}
	trade, found := s.GetTrade(tradeID)
	if !found {
		return nil, errors.New("sandbox trade unavailable")
	}
	payload, err := json.Marshal(trade.PricingSnapshot)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(payload)
	command := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, EventID: "sbx:reserve:" + trade.ID, Event: "reserve", Actor: "sbx:trade-orchestrator", CorrelationID: trade.CorrelationID, Reason: "accepted sandbox FX quote source reservation", EvidencePayload: string(payload), EvidenceHash: hex.EncodeToString(hash[:]), Terms: terms}
	raw, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	receipt, err := client.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(raw)})
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.EventId != command.EventID || receipt.State != "HELD" || receipt.Revision != 1 {
		return nil, errors.New("sandbox source reservation receipt mismatch")
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	return s.createFXExecutionWithReservationDB(tradeID, "sbx:fx:"+tradeID, executor, &sandboxFXReservation{event: command.EventID, revision: receipt.Revision, command: string(raw), receipt: string(receiptRaw)})
}

// ProcessSandboxFXAcceptedBatch submits only the first durably funded intent.
// A verified NOT_EXECUTED outcome never authorizes an automatic resubmission.
// Completed instructions queue Ledger posting against their stored hold receipt.
func (s *Store) ProcessSandboxFXAcceptedBatch(ctx context.Context, client *SandboxFXClient, limit int) (int, error) {
	enabled, err := sandboxpolicy.FXAcceptance()
	if err != nil || !enabled {
		return 0, err
	}
	if client == nil || limit < 1 || limit > 100 {
		return 0, errors.New("bounded sandbox acceptance batch required")
	}
	if err := s.validateSandboxPosting(ctx); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.trade_id,e.status,r.revision FROM trade_fx_executions e JOIN trade_sandbox_fx_reservations r ON r.trade_id=e.trade_id LEFT JOIN trade_sandbox_fx_postings p ON p.execution_id=e.execution_id WHERE e.executor_id=$1 AND ((e.status='READY' AND (e.body->>'submission_count')::int=0) OR (e.status='EXECUTED' AND p.event_id IS NULL)) ORDER BY COALESCE(r.attempted_at,e.updated_at),e.execution_id LIMIT $2`, client.executorID, limit)
	if err != nil {
		return 0, err
	}
	type job struct {
		trade, status string
		revision      int64
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.trade, &j.status, &j.revision); err != nil {
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
	var failures []error
	processed := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE trade_sandbox_fx_reservations SET attempted_at=now() WHERE trade_id=$1`, j.trade); err != nil {
			failures = append(failures, err)
			continue
		}
		if j.status == string(FXExecutionReady) {
			_, err = s.submitFXExecution(ctx, j.trade, client, client, true)
		} else {
			err = s.EnqueueSandboxFXPosting(ctx, j.trade, j.revision)
		}
		if err != nil {
			failures = append(failures, err)
		} else {
			processed++
		}
	}
	return processed, errors.Join(failures...)
}
