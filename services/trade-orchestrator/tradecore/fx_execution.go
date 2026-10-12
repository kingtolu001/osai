package tradecore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/pricing"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"github.com/osai/osai/services/ledger/ledgercore"
)

type FXExecutionStatus string

const (
	FXExecutionReady     FXExecutionStatus = "READY"
	FXExecutionSubmitted FXExecutionStatus = "SUBMITTED"
	FXExecutionUnknown   FXExecutionStatus = "UNKNOWN"
	FXExecutionComplete  FXExecutionStatus = "EXECUTED"
	FXExecutionFailed    FXExecutionStatus = "FAILED"
)

type FXExecution struct {
	ID                     string                          `json:"id"`
	TradeID                string                          `json:"trade_id"`
	InstitutionID          string                          `json:"institution_id"`
	QuoteID                string                          `json:"quote_id"`
	IdempotencyKey         string                          `json:"idempotency_key"`
	RequestHash            string                          `json:"request_hash"`
	ExecutorID             string                          `json:"executor_id"`
	RequestReference       string                          `json:"request_reference"`
	Status                 FXExecutionStatus               `json:"status"`
	SourceCurrency         string                          `json:"source_currency"`
	SourceExponent         uint32                          `json:"source_exponent"`
	SourcePrincipalMinor   int64                           `json:"source_principal_minor"`
	DestinationCurrency    string                          `json:"destination_currency"`
	DestinationExponent    uint32                          `json:"destination_exponent"`
	QuotedDestinationMinor int64                           `json:"quoted_destination_minor"`
	QuotedRateScaled       int64                           `json:"quoted_rate_scaled"`
	QuotedRateDirection    string                          `json:"quoted_rate_direction"`
	PricingSnapshotHash    string                          `json:"pricing_snapshot_hash"`
	SubmissionCount        int                             `json:"submission_count"`
	RecoveryCount          int                             `json:"recovery_count"`
	LastRecoveryEvidence   *FXQueryEvidence                `json:"last_recovery_evidence,omitempty"`
	ExecutionEvidence      *ledgercore.FXExecutionEvidence `json:"execution_evidence,omitempty"`
	FailureEvidence        *FXQueryEvidence                `json:"failure_evidence,omitempty"`
	CreatedAt              time.Time                       `json:"created_at"`
	UpdatedAt              time.Time                       `json:"updated_at"`
}

type FXQueryStatus string

const (
	FXQueryPending     FXQueryStatus = "PENDING"
	FXQueryNotExecuted FXQueryStatus = "NOT_EXECUTED"
	FXQueryExecuted    FXQueryStatus = "EXECUTED"
	FXQueryFailed      FXQueryStatus = "FAILED"
	FXQueryUnknown     FXQueryStatus = "UNKNOWN"
)

type FXQueryEvidence struct {
	ExecutorID       string    `json:"executor_id"`
	RequestReference string    `json:"request_reference"`
	ProviderRef      string    `json:"provider_reference"`
	QueryReference   string    `json:"query_reference"`
	PayloadHash      string    `json:"payload_hash"`
	ObservedAt       time.Time `json:"observed_at"`
	Authoritative    bool      `json:"authoritative"`
}

type FXQueryResult struct {
	Status        FXQueryStatus                   `json:"status"`
	QueryEvidence FXQueryEvidence                 `json:"query_evidence"`
	Execution     *ledgercore.FXExecutionEvidence `json:"execution,omitempty"`
}

type FXExecutionExecutor interface {
	SubmitFX(context.Context, FXExecution) error
	QueryFX(context.Context, FXExecution) (FXQueryResult, error)
}

func (s *Store) CreateFXExecution(tradeID, idempotencyKey, executorID string) (*FXExecution, error) {
	if strings.TrimSpace(tradeID) == "" || strings.TrimSpace(idempotencyKey) == "" || strings.TrimSpace(executorID) == "" {
		return nil, errors.New("trade, idempotency key, and FX executor are required")
	}
	if s.db == nil {
		return nil, errors.New("durable Trade persistence is required for FX execution")
	}
	return s.createFXExecutionDB(tradeID, idempotencyKey, executorID)
}

func (s *Store) GetFXExecution(tradeID string) (*FXExecution, bool, error) {
	if s.db == nil {
		return nil, false, errors.New("durable Trade persistence is required for FX execution")
	}
	var raw []byte
	err := s.db.QueryRow(`SELECT body FROM trade_fx_executions WHERE trade_id=$1`, tradeID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var execution FXExecution
	if err := json.Unmarshal(raw, &execution); err != nil {
		return nil, false, err
	}
	return &execution, true, nil
}

func (s *Store) ListFXExecutionsForRecovery(limit int) ([]FXExecution, error) {
	if s.db == nil {
		return nil, errors.New("durable Trade persistence is required for FX execution")
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("recovery limit must be between 1 and 100")
	}
	rows, err := s.db.Query(`SELECT body FROM trade_fx_executions
		WHERE status IN ('SUBMITTED','UNKNOWN') ORDER BY updated_at,execution_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	executions := make([]FXExecution, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var execution FXExecution
		if err := json.Unmarshal(raw, &execution); err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	return executions, rows.Err()
}

func (s *Store) MarkFXExecutionSubmitted(tradeID string) (*FXExecution, error) {
	return s.mutateFXExecution(tradeID, func(execution *FXExecution, trade *Trade) (string, error) {
		if execution.Status == FXExecutionSubmitted {
			return "", errors.New("FX execution already submitted; query before any retry")
		}
		if execution.Status != FXExecutionReady {
			return "", fmt.Errorf("FX execution cannot be submitted from %s; reconcile first", execution.Status)
		}
		execution.Status = FXExecutionSubmitted
		execution.SubmissionCount++
		trade.State = StateFXExecuting
		return "trade.fx_execution_submitted", nil
	})
}

func (s *Store) MarkFXExecutionUnknown(tradeID string) (*FXExecution, error) {
	return s.mutateFXExecution(tradeID, func(execution *FXExecution, trade *Trade) (string, error) {
		if execution.Status == FXExecutionUnknown {
			return "", nil
		}
		if execution.Status != FXExecutionSubmitted {
			return "", fmt.Errorf("only a submitted FX execution can become unknown, got %s", execution.Status)
		}
		execution.Status = FXExecutionUnknown
		trade.State = StateFXUnknown
		return "trade.fx_execution_unknown", nil
	})
}

func (s *Store) ApplyFXQueryResult(tradeID string, result FXQueryResult) (*FXExecution, error) {
	return s.mutateFXExecution(tradeID, func(execution *FXExecution, trade *Trade) (string, error) {
		if execution.Status != FXExecutionSubmitted && execution.Status != FXExecutionUnknown {
			return "", fmt.Errorf("FX query recovery is not allowed from %s", execution.Status)
		}
		if err := validateFXQueryEvidence(execution, result.QueryEvidence); err != nil {
			return "", err
		}
		switch result.Status {
		case FXQueryUnknown:
			execution.Status = FXExecutionUnknown
			execution.LastRecoveryEvidence = cloneFXQueryEvidence(&result.QueryEvidence)
			execution.RecoveryCount++
			trade.State = StateFXUnknown
			return "trade.fx_execution_unknown", nil
		case FXQueryPending:
			execution.LastRecoveryEvidence = cloneFXQueryEvidence(&result.QueryEvidence)
			execution.RecoveryCount++
			return "trade.fx_execution_pending", nil
		case FXQueryNotExecuted:
			if !result.QueryEvidence.Authoritative {
				return "", errors.New("non-execution requires authoritative query evidence")
			}
			execution.Status = FXExecutionReady
			execution.LastRecoveryEvidence = cloneFXQueryEvidence(&result.QueryEvidence)
			execution.RecoveryCount++
			trade.State = StateFXExecutionRequired
			return "trade.fx_execution_retry_ready", nil
		case FXQueryExecuted:
			if !result.QueryEvidence.Authoritative {
				return "", errors.New("execution requires authoritative query evidence")
			}
			if result.Execution == nil {
				return "", errors.New("verified execution evidence is required")
			}
			if result.QueryEvidence.ProviderRef != result.Execution.ExecutionReference {
				return "", errors.New("FX query reference does not match execution evidence")
			}
			if err := validateFXExecution(execution, *result.Execution); err != nil {
				return "", err
			}
			execution.Status = FXExecutionComplete
			execution.ExecutionEvidence = cloneFXExecutionEvidence(result.Execution)
			execution.LastRecoveryEvidence = cloneFXQueryEvidence(&result.QueryEvidence)
			execution.RecoveryCount++
			trade.State = StateFXExecuted
			return "trade.fx_executed", nil
		case FXQueryFailed:
			if !result.QueryEvidence.Authoritative {
				return "", errors.New("terminal failure requires authoritative query evidence")
			}
			execution.Status = FXExecutionFailed
			execution.FailureEvidence = cloneFXQueryEvidence(&result.QueryEvidence)
			execution.RecoveryCount++
			trade.State = StateFXFailed
			return "trade.fx_execution_failed", nil
		default:
			return "", errors.New("unsupported FX query result")
		}
	})
}

// SubmitFXExecution persists SUBMITTED and its stable request reference before
// calling the executor. Every submission error becomes UNKNOWN; retries require
// a query proving that the previous request did not execute.
func (s *Store) SubmitFXExecution(ctx context.Context, tradeID string, executor FXExecutionExecutor) (*FXExecution, error) {
	if executor == nil {
		return nil, errors.New("FX executor is required")
	}
	execution, found, err := s.GetFXExecution(tradeID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("FX execution not found")
	}
	if execution.Status != FXExecutionReady {
		return nil, fmt.Errorf("FX execution must be READY before submit, got %s", execution.Status)
	}
	submitted, err := s.MarkFXExecutionSubmitted(tradeID)
	if err != nil {
		return nil, err
	}
	if err = executor.SubmitFX(ctx, *submitted); err != nil {
		unknown, markErr := s.MarkFXExecutionUnknown(tradeID)
		if markErr != nil {
			return nil, fmt.Errorf("submission outcome uncertain (%v); persist UNKNOWN state: %w", err, markErr)
		}
		return unknown, fmt.Errorf("FX submission outcome uncertain; query before retry: %w", err)
	}
	return s.RecoverFXExecution(ctx, tradeID, executor)
}

func (s *Store) RecoverFXExecution(ctx context.Context, tradeID string, executor FXExecutionExecutor) (*FXExecution, error) {
	if executor == nil {
		return nil, errors.New("FX executor is required")
	}
	execution, found, err := s.GetFXExecution(tradeID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("FX execution not found")
	}
	if execution.Status != FXExecutionSubmitted && execution.Status != FXExecutionUnknown {
		return nil, fmt.Errorf("FX query is not allowed from %s", execution.Status)
	}
	result, err := executor.QueryFX(ctx, *execution)
	if err != nil {
		if execution.Status == FXExecutionSubmitted {
			if _, markErr := s.MarkFXExecutionUnknown(tradeID); markErr != nil {
				return nil, fmt.Errorf("FX query failed (%v); persist UNKNOWN state: %w", err, markErr)
			}
		}
		return nil, fmt.Errorf("FX query failed; execution remains unresolved: %w", err)
	}
	return s.ApplyFXQueryResult(tradeID, result)
}

func (s *Store) mutateFXExecution(tradeID string, update func(*FXExecution, *Trade) (string, error)) (*FXExecution, error) {
	if s.db == nil {
		return nil, errors.New("durable Trade persistence is required for FX execution")
	}
	return s.mutateFXExecutionDB(tradeID, update)
}

func (s *Store) createFXExecutionDB(tradeID, idempotencyKey, executorID string) (*FXExecution, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM trades WHERE id=$1 FOR UPDATE`, tradeID).Scan(&raw); err != nil {
		return nil, err
	}
	var trade Trade
	if err = json.Unmarshal(raw, &trade); err != nil {
		return nil, err
	}
	if trade.PricingSnapshot == nil {
		return nil, errors.New("immutable pricing snapshot required")
	}
	if err = pricing.Validate(*trade.PricingSnapshot); err != nil {
		return nil, fmt.Errorf("invalid FX pricing snapshot: %w", err)
	}
	hash, err := fxExecutionRequestHash(&trade, idempotencyKey, executorID)
	if err != nil {
		return nil, err
	}
	var existingRaw []byte
	err = tx.QueryRow(`SELECT body FROM trade_fx_executions WHERE trade_id=$1 FOR UPDATE`, tradeID).Scan(&existingRaw)
	if err == nil {
		var existing FXExecution
		if err = json.Unmarshal(existingRaw, &existing); err != nil {
			return nil, err
		}
		if existing.IdempotencyKey != idempotencyKey || existing.RequestHash != hash {
			return nil, errors.New("FX execution idempotency conflict")
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if trade.State != StateFunded || trade.PricingSnapshot.SourceCurrency == trade.PricingSnapshot.DestinationCurrency {
		return nil, errors.New("funded cross-currency trade with immutable pricing is required")
	}
	execution := newFXExecution(&trade, idempotencyKey, executorID, hash)
	trade.FXExecutionID = execution.ID
	trade.FXExecutorID = execution.ExecutorID
	trade.FXExecutionReference = execution.RequestReference
	trade.FXExecutionStatus = string(execution.Status)
	previous := trade.Status
	trade.State, trade.Status, trade.UpdatedAt = StateFXExecutionRequired, string(StateFXExecutionRequired), execution.CreatedAt
	if err = saveFXState(tx, &trade, execution, previous, "trade.fx_execution_required"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return execution, nil
}

func (s *Store) mutateFXExecutionDB(tradeID string, update func(*FXExecution, *Trade) (string, error)) (*FXExecution, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var executionRaw, tradeRaw []byte
	if err = tx.QueryRow(`SELECT body FROM trades WHERE id=$1 FOR UPDATE`, tradeID).Scan(&tradeRaw); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(`SELECT body FROM trade_fx_executions WHERE trade_id=$1 FOR UPDATE`, tradeID).Scan(&executionRaw); err != nil {
		return nil, err
	}
	var execution FXExecution
	var trade Trade
	if err = json.Unmarshal(executionRaw, &execution); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(tradeRaw, &trade); err != nil {
		return nil, err
	}
	previous := trade.Status
	eventType, err := update(&execution, &trade)
	if err != nil {
		return nil, err
	}
	if eventType == "" {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &execution, nil
	}
	trade.FXExecutionStatus = string(execution.Status)
	trade.Status, trade.UpdatedAt = string(trade.State), time.Now().UTC()
	execution.UpdatedAt = trade.UpdatedAt
	if err = saveFXState(tx, &trade, &execution, previous, eventType); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &execution, nil
}

func saveFXState(tx *sql.Tx, trade *Trade, execution *FXExecution, previousTradeStatus, eventType string) error {
	tradeRaw, err := json.Marshal(trade)
	if err != nil {
		return err
	}
	executionRaw, err := json.Marshal(execution)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO trade_fx_executions(execution_id,trade_id,institution_id,executor_id,request_reference,status,body,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(trade_id) DO UPDATE SET executor_id=EXCLUDED.executor_id,request_reference=EXCLUDED.request_reference,status=EXCLUDED.status,body=EXCLUDED.body,updated_at=EXCLUDED.updated_at`,
		execution.ID, execution.TradeID, execution.InstitutionID, execution.ExecutorID, execution.RequestReference, execution.Status, string(executionRaw), execution.CreatedAt, execution.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE trades SET body=$2,status=$3 WHERE id=$1`, trade.ID, string(tradeRaw), trade.Status); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO trade_transitions(trade_id,from_state,to_state) VALUES($1,$2,$3)`, trade.ID, previousTradeStatus, trade.Status); err != nil {
		return err
	}
	return insertFXTradeEvent(tx, trade, execution, eventType)
}

func insertFXTradeEvent(tx *sql.Tx, trade *Trade, execution *FXExecution, eventType string) error {
	payload, err := json.Marshal(map[string]any{
		"trade_id": trade.ID, "quote_id": trade.QuoteID,
		"execution_id": execution.ID, "executor_id": execution.ExecutorID,
		"request_reference": execution.RequestReference, "execution_status": execution.Status,
	})
	if err != nil {
		return err
	}
	attempt := execution.SubmissionCount + execution.RecoveryCount
	eventSuffix := strings.ReplaceAll(strings.TrimPrefix(eventType, "trade.fx_"), ".", "_")
	event := notificationv1.Event{
		EventId:       fmt.Sprintf("evt_trade_%s_fx_%s_%d", trade.ID, eventSuffix, attempt),
		InstitutionId: trade.InstitutionID, EventType: eventType, CorrelationId: trade.CorrelationID,
		AggregateId: trade.ID, OccurredAt: trade.UpdatedAt.UTC().Format(time.RFC3339Nano), PayloadJson: string(payload),
	}
	raw, err := json.Marshal(&event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO trade_outbox(event_id,institution_id,trade_id,event_type,body) VALUES($1,$2,$3,$4,$5) ON CONFLICT(event_id) DO NOTHING`,
		event.EventId, trade.InstitutionID, trade.ID, eventType, string(raw))
	return err
}

func newFXExecution(trade *Trade, idempotencyKey, executorID, requestHash string) *FXExecution {
	now := time.Now().UTC()
	snapshot := trade.PricingSnapshot
	return &FXExecution{
		ID: "fxe_" + uuid.NewString(), TradeID: trade.ID, InstitutionID: trade.InstitutionID,
		QuoteID: trade.QuoteID, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		ExecutorID: executorID, RequestReference: "fxr_" + uuid.NewString(), Status: FXExecutionReady,
		SourceCurrency: snapshot.SourceCurrency, SourceExponent: snapshot.SourceExponent,
		SourcePrincipalMinor: snapshot.SourcePrincipalMinor, DestinationCurrency: snapshot.DestinationCurrency,
		DestinationExponent: snapshot.DestinationExponent, QuotedDestinationMinor: snapshot.DestinationAmountMinor,
		QuotedRateScaled: snapshot.RateScaled, QuotedRateDirection: snapshot.RateDirection,
		PricingSnapshotHash: snapshot.Hash, CreatedAt: now, UpdatedAt: now,
	}
}

func fxExecutionRequestHash(trade *Trade, idempotencyKey, executorID string) (string, error) {
	if trade.PricingSnapshot == nil {
		return "", errors.New("immutable pricing snapshot required")
	}
	canonical := struct {
		TradeID, InstitutionID, QuoteID, IdempotencyKey, ExecutorID, PricingHash string
	}{trade.ID, trade.InstitutionID, trade.QuoteID, idempotencyKey, executorID, trade.PricingSnapshot.Hash}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateFXQueryEvidence(execution *FXExecution, evidence FXQueryEvidence) error {
	if evidence.ExecutorID != execution.ExecutorID || evidence.RequestReference != execution.RequestReference ||
		strings.TrimSpace(evidence.QueryReference) == "" || len(evidence.PayloadHash) != 64 || evidence.ObservedAt.IsZero() {
		return errors.New("FX query evidence identity is incomplete or mismatched")
	}
	if _, err := hex.DecodeString(evidence.PayloadHash); err != nil {
		return errors.New("FX query evidence payload hash must be SHA-256 hex")
	}
	return nil
}

func validateFXExecution(execution *FXExecution, evidence ledgercore.FXExecutionEvidence) error {
	if err := ledgercore.ValidateFXExecutionEvidence(evidence); err != nil {
		return err
	}
	if evidence.TradeID != execution.TradeID || evidence.InstitutionID != execution.InstitutionID ||
		evidence.QuoteID != execution.QuoteID || evidence.Executor != execution.ExecutorID ||
		evidence.SourceCurrency != execution.SourceCurrency || evidence.SourceExponent != uint8(execution.SourceExponent) ||
		evidence.SourcePrincipalMinor != execution.SourcePrincipalMinor ||
		evidence.DestinationCurrency != execution.DestinationCurrency ||
		evidence.DestinationExponent != uint8(execution.DestinationExponent) {
		return errors.New("FX execution evidence does not match accepted trade economics")
	}
	return nil
}

func cloneFXExecution(execution *FXExecution) *FXExecution {
	if execution == nil {
		return nil
	}
	copy := *execution
	copy.LastRecoveryEvidence = cloneFXQueryEvidence(execution.LastRecoveryEvidence)
	copy.FailureEvidence = cloneFXQueryEvidence(execution.FailureEvidence)
	copy.ExecutionEvidence = cloneFXExecutionEvidence(execution.ExecutionEvidence)
	return &copy
}

func cloneFXQueryEvidence(evidence *FXQueryEvidence) *FXQueryEvidence {
	if evidence == nil {
		return nil
	}
	copy := *evidence
	return &copy
}

func cloneFXExecutionEvidence(evidence *ledgercore.FXExecutionEvidence) *ledgercore.FXExecutionEvidence {
	if evidence == nil {
		return nil
	}
	copy := *evidence
	copy.Fees = append([]ledgercore.FXFeeEvidence(nil), evidence.Fees...)
	return &copy
}
