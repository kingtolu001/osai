package settlementstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osai/osai/pkg/ids"
	"github.com/osai/osai/pkg/provider"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"google.golang.org/grpc/metadata"
)

type Postgres struct{ DB *sql.DB }

type FinalityEvent struct {
	Supersedes    string
	EventID       string
	InstitutionID string
	SettlementID  string
	TradeID       string
	Status        string
	CorrelationID string
	OccurredAt    time.Time
}

type DomainEvent struct {
	EventID       string
	InstitutionID string
	SettlementID  string
	TradeID       string
	EventType     string
	CorrelationID string
	OccurredAt    time.Time
}

type CustomerSettlementReportItem struct {
	SettlementID string
	TradeID      string
	QuoteID      string
	Status       string
	AmountMinor  int64
	Currency     string
	OccurredAt   time.Time
}

func (p Postgres) ListCustomerSettlementReport(ctx context.Context, institutionID string, from, to time.Time, currency, status string, page, pageSize int) ([]CustomerSettlementReportItem, int, error) {
	if institutionID == "" || page < 1 || pageSize < 1 || pageSize > 100 ||
		(!from.IsZero() && !to.IsZero() && !from.Before(to)) ||
		(status != "" && status != string(settlementcore.Confirmed) && status != string(settlementcore.Failed) && status != string(settlementcore.Returned) && status != string(settlementcore.Reversed)) {
		return nil, 0, errors.New("invalid customer settlement report query")
	}
	var fromValue, toValue any
	if !from.IsZero() {
		fromValue = from
	}
	if !to.IsZero() {
		toValue = to
	}
	args := []any{institutionID, currency, status, fromValue, toValue}
	const queryFilter = `i.institution_id=$1 AND i.trade_id IS NOT NULL AND i.trade_id<>'' AND i.state IN ('CONFIRMED','FAILED','RETURNED','REVERSED') AND ($2='' OR upper(i.currency)=upper($2)) AND ($3='' OR i.state=$3) AND ($4::timestamptz IS NULL OR t.occurred_at >= $4) AND ($5::timestamptz IS NULL OR t.occurred_at < $5)`
	var total int
	if err := p.DB.QueryRowContext(ctx, `SELECT count(*) FROM settlement_instructions i JOIN LATERAL (SELECT occurred_at FROM settlement_transitions WHERE settlement_id=i.id AND to_state=i.state ORDER BY occurred_at DESC LIMIT 1) t ON true WHERE `+queryFilter, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	rows, err := p.DB.QueryContext(ctx, `SELECT i.id,COALESCE(i.trade_id,''),i.quote_id,i.state,i.amount_minor,i.currency,t.occurred_at FROM settlement_instructions i JOIN LATERAL (SELECT occurred_at FROM settlement_transitions WHERE settlement_id=i.id AND to_state=i.state ORDER BY occurred_at DESC LIMIT 1) t ON true WHERE `+queryFilter+` ORDER BY t.occurred_at DESC,i.id DESC LIMIT $6 OFFSET $7`, append(args, pageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]CustomerSettlementReportItem, 0)
	for rows.Next() {
		var item CustomerSettlementReportItem
		if err := rows.Scan(&item.SettlementID, &item.TradeID, &item.QuoteID, &item.Status, &item.AmountMinor, &item.Currency, &item.OccurredAt); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// ListConfirmedForReconciliation exposes an ordered, bounded snapshot through
// the settlement service; reconciliation never reads settlement tables itself.
func (p Postgres) ListConfirmedForReconciliation(ctx context.Context, providerID string, from, to time.Time, afterID string, limit int) ([]settlementcore.Instruction, error) {
	if providerID == "" || !to.After(from) || limit < 1 || limit > 100 {
		return nil, errors.New("invalid reconciliation window")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT id,client_ref,provider_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,COALESCE(trade_id,''),quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions WHERE provider_id=$1 AND state='CONFIRMED' AND updated_at >= $2 AND updated_at < $3 AND id > $4 ORDER BY id LIMIT $5`, providerID, from, to, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]settlementcore.Instruction, 0)
	for rows.Next() {
		var item settlementcore.Instruction
		var state string
		if err := rows.Scan(&item.ID, &item.ClientRef, &item.ProviderRef, &item.Beneficiary, &item.AmountMinor, &item.Currency, &item.Purpose, &state, &item.CreatedAt, &item.UpdatedAt, &item.InstitutionID, &item.TradeID, &item.QuoteID, &item.CorrelationID, &item.ProviderID, &item.BankCode, &item.AccountNumber); err != nil {
			return nil, err
		}
		item.State = settlementcore.State(state)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (p Postgres) CreateForTrade(ctx context.Context, institutionID, tradeID, quoteID, correlationID, beneficiary, currency string, amountMinor int64, purpose, providerID, bankCode, accountNumber string) (settlementcore.Instruction, error) {
	if institutionID == "" || tradeID == "" || quoteID == "" || amountMinor <= 0 {
		return settlementcore.Instruction{}, errors.New("invalid settlement command")
	}
	now := time.Now().UTC()
	id := ids.NewSettlementID()
	ref := ids.NewSettlementID()
	_, err := p.DB.ExecContext(ctx, `INSERT INTO settlement_instructions(id,client_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number) VALUES($1,$2,$3,$4,$5,$6,'CREATED',$7,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (trade_id) WHERE trade_id IS NOT NULL AND (supersedes IS NULL OR supersedes='') DO NOTHING`, id, ref, beneficiary, amountMinor, currency, purpose, now, institutionID, tradeID, quoteID, correlationID, providerID, bankCode, accountNumber)
	if err != nil {
		return settlementcore.Instruction{}, err
	}
	var i settlementcore.Instruction
	var state string
	err = p.DB.QueryRowContext(ctx, `SELECT id,client_ref,provider_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions WHERE trade_id=$1 AND (supersedes IS NULL OR supersedes='')`, tradeID).Scan(&i.ID, &i.ClientRef, &i.ProviderRef, &i.Beneficiary, &i.AmountMinor, &i.Currency, &i.Purpose, &state, &i.CreatedAt, &i.UpdatedAt, &i.InstitutionID, &i.TradeID, &i.QuoteID, &i.CorrelationID, &i.ProviderID, &i.BankCode, &i.AccountNumber)
	if err != nil {
		return settlementcore.Instruction{}, err
	}
	i.State = settlementcore.State(state)
	if i.InstitutionID != institutionID || i.QuoteID != quoteID || i.AmountMinor != amountMinor || i.Currency != currency || i.ProviderID != providerID || i.BankCode != bankCode || i.AccountNumber != accountNumber || i.Beneficiary != beneficiary || i.Purpose != purpose || i.CorrelationID != correlationID {
		return settlementcore.Instruction{}, errors.New("settlement replay conflict")
	}
	return i, nil
}

func (p Postgres) SaveInstruction(instruction settlementcore.Instruction) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_instructions (id, client_ref, provider_ref, beneficiary, amount_minor, currency, purpose, state, supersedes, created_at, updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14,$15,$16,$17,$18) ON CONFLICT (id) DO UPDATE SET provider_ref=EXCLUDED.provider_ref, state=EXCLUDED.state, supersedes=EXCLUDED.supersedes, updated_at=EXCLUDED.updated_at`, instruction.ID, instruction.ClientRef, instruction.ProviderRef, instruction.Beneficiary, instruction.AmountMinor, instruction.Currency, instruction.Purpose, instruction.State, instruction.Supersedes, instruction.CreatedAt, instruction.UpdatedAt, instruction.InstitutionID, instruction.TradeID, instruction.QuoteID, instruction.CorrelationID, instruction.ProviderID, instruction.BankCode, instruction.AccountNumber)
	return err
}

func (p Postgres) SaveTransition(id string, transition settlementcore.Transition) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_transitions (settlement_id, from_state, to_state, event, reason, source, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, transition.From, transition.To, transition.Event, transition.Reason, transition.Source, transition.OccurredAt)
	return err
}

func (p Postgres) SaveEvidence(id string, evidence provider.Evidence) error {
	_, err := p.DB.Exec(`INSERT INTO settlement_evidence (evidence_id, settlement_id, provider, method, request_ref, raw_payload_hash, parsed_at, correlation_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (evidence_id) DO NOTHING`, evidence.ID, id, evidence.Provider, evidence.Method, evidence.RequestRef, evidence.RawPayloadHash, evidence.ParsedAt, evidence.CorrelationID)
	return err
}

func (p Postgres) LoadInstructions() ([]settlementcore.Instruction, error) {
	rows, err := p.DB.Query(`SELECT id, client_ref, provider_ref, beneficiary, amount_minor, currency, purpose, state, supersedes, created_at, updated_at,institution_id,COALESCE(trade_id,''),quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]settlementcore.Instruction, 0)
	for rows.Next() {
		var instruction settlementcore.Instruction
		var state string
		var supersedes sql.NullString
		if err := rows.Scan(&instruction.ID, &instruction.ClientRef, &instruction.ProviderRef, &instruction.Beneficiary, &instruction.AmountMinor, &instruction.Currency, &instruction.Purpose, &state, &supersedes, &instruction.CreatedAt, &instruction.UpdatedAt, &instruction.InstitutionID, &instruction.TradeID, &instruction.QuoteID, &instruction.CorrelationID, &instruction.ProviderID, &instruction.BankCode, &instruction.AccountNumber); err != nil {
			return nil, err
		}
		instruction.State = settlementcore.State(state)
		instruction.Supersedes = supersedes.String
		result = append(result, instruction)
	}
	return result, rows.Err()
}

var _ settlementcore.Persistence = Postgres{}
var _ = time.Time{}

// RecordRuntimeState persists the Temporal worker's provider evidence and SI
// transition without sharing ownership with the ledger service.
func (p Postgres) RecordRuntimeState(ctx context.Context, id string, next settlementcore.State, providerRef string, evidence provider.Evidence) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, existingRef, institutionID, tradeID, correlationID string
	if err = tx.QueryRowContext(ctx, `SELECT state,provider_ref,institution_id,COALESCE(trade_id,''),correlation_id FROM settlement_instructions WHERE id=$1 FOR UPDATE`, id).Scan(&current, &existingRef, &institutionID, &tradeID, &correlationID); err != nil {
		return err
	}
	if settlementcore.State(current).Terminal() {
		return tx.Commit()
	}
	if current == string(settlementcore.ManualReview) || next == settlementcore.Submitted && current != string(settlementcore.Created) {
		return tx.Commit()
	}
	if providerRef != "" && existingRef != "" && existingRef != providerRef {
		return errors.New("settlement provider reference conflict")
	}
	if providerRef == "" {
		providerRef = existingRef
	}
	if current != string(next) || providerRef != existingRef {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_instructions SET state=$2,provider_ref=$3,updated_at=now() WHERE id=$1`, id, next, providerRef); err != nil {
			return err
		}
		if current != string(next) {
			var transitionID int64
			var occurredAt time.Time
			if err = tx.QueryRowContext(ctx, `INSERT INTO settlement_transitions(settlement_id,from_state,to_state,event,reason,source,occurred_at) VALUES($1,$2,$3,$4,$5,'provider',now()) RETURNING id,occurred_at`, id, current, next, "provider_runtime", "provider status evidence").Scan(&transitionID, &occurredAt); err != nil {
				return err
			}
			eventType := map[settlementcore.State]string{
				settlementcore.Processing: "trade.settlement_processing",
				settlementcore.Confirmed:  "trade.settled",
			}[next]
			if eventType != "" && tradeID != "" {
				eventID := fmt.Sprintf("settlement:%s:%d", eventType, transitionID)
				if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_domain_outbox(event_id,institution_id,settlement_id,trade_id,event_type,correlation_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO NOTHING`, eventID, institutionID, id, tradeID, eventType, correlationID, occurredAt); err != nil {
					return err
				}
			}
		}
	}
	if evidence.ID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_evidence(evidence_id,settlement_id,provider,method,request_ref,raw_payload_hash,parsed_at,correlation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(evidence_id) DO NOTHING`, evidence.ID, id, evidence.Provider, evidence.Method, evidence.RequestRef, evidence.RawPayloadHash, evidence.ParsedAt, evidence.CorrelationID)
		if err != nil {
			return err
		}
	}
	if (next == settlementcore.Confirmed || next == settlementcore.Failed) && tradeID != "" {
		eventID := "settlement_finality:" + id + ":" + string(next)
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_finality_outbox(event_id,institution_id,settlement_id,trade_id,status,correlation_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,now()) ON CONFLICT(event_id) DO NOTHING`, eventID, institutionID, id, tradeID, string(next), correlationID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p Postgres) RelayDomainEvents(ctx context.Context, client notificationv1.NotificationServiceClient, token string) error {
	if client == nil || token == "" {
		return errors.New("notification client and token are required")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT event_id,institution_id,settlement_id,trade_id,event_type,correlation_id,occurred_at FROM settlement_domain_outbox WHERE delivered_at IS NULL ORDER BY occurred_at,event_id LIMIT 50`)
	if err != nil {
		return err
	}
	events := make([]DomainEvent, 0, 50)
	for rows.Next() {
		var event DomainEvent
		if err := rows.Scan(&event.EventID, &event.InstitutionID, &event.SettlementID, &event.TradeID, &event.EventType, &event.CorrelationID, &event.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, event := range events {
		customerStatus := "PROCESSING"
		if event.EventType == "trade.settled" {
			customerStatus = "SETTLED"
		}
		if event.EventType == "si.unknown_escalated" {
			customerStatus = "UNKNOWN"
		}
		if event.EventType == "trade.returned" {
			customerStatus = "RETURNED"
		}
		if event.EventType == "trade.reversed" {
			customerStatus = "REVERSED"
		}
		payload, err := json.Marshal(map[string]string{
			"settlement_id": event.SettlementID,
			"trade_id":      event.TradeID,
			"status":        customerStatus,
		})
		if err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), 5*time.Second)
		_, err = client.PublishEvent(callCtx, &notificationv1.Event{
			EventId: event.EventID, InstitutionId: event.InstitutionID, EventType: event.EventType,
			CorrelationId: event.CorrelationID, AggregateId: event.TradeID,
			OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano), PayloadJson: string(payload),
		})
		cancel()
		if err != nil {
			return err
		}
		if _, err = p.DB.ExecContext(ctx, `UPDATE settlement_domain_outbox SET delivered_at=COALESCE(delivered_at,now()) WHERE event_id=$1`, event.EventID); err != nil {
			return err
		}
	}
	return nil
}

func (p Postgres) ListPendingFinalityEvents(ctx context.Context, limit int) ([]FinalityEvent, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("invalid finality event limit")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT f.event_id,f.institution_id,f.settlement_id,f.trade_id,f.status,f.correlation_id,f.occurred_at,f.supersedes FROM settlement_finality_outbox f WHERE f.delivered_at IS NULL
 AND (f.status<>'RETRYING' OR EXISTS (SELECT 1 FROM settlement_finality_outbox prior WHERE prior.settlement_id=f.supersedes AND prior.status='FAILED' AND prior.delivered_at IS NOT NULL))
 AND (f.status='RETRYING' OR NOT EXISTS (SELECT 1 FROM settlement_finality_outbox retry WHERE retry.settlement_id=f.settlement_id AND retry.status='RETRYING' AND retry.delivered_at IS NULL))
 AND (f.status NOT IN ('RETURNED','REVERSED') OR EXISTS (SELECT 1 FROM settlement_finality_outbox confirmed WHERE confirmed.settlement_id=f.settlement_id AND confirmed.status='CONFIRMED' AND confirmed.delivered_at IS NOT NULL))
 ORDER BY f.occurred_at,f.event_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]FinalityEvent, 0)
	for rows.Next() {
		var event FinalityEvent
		if err := rows.Scan(&event.EventID, &event.InstitutionID, &event.SettlementID, &event.TradeID, &event.Status, &event.CorrelationID, &event.OccurredAt, &event.Supersedes); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (p Postgres) AcknowledgeFinalityEvent(ctx context.Context, eventID string) error {
	if eventID == "" {
		return errors.New("event_id required")
	}
	result, err := p.DB.ExecContext(ctx, `UPDATE settlement_finality_outbox SET delivered_at=COALESCE(delivered_at,now()) WHERE event_id=$1`, eventID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (p Postgres) GetByID(ctx context.Context, id string) (settlementcore.Instruction, error) {
	var i settlementcore.Instruction
	var state string
	err := p.DB.QueryRowContext(ctx, `SELECT id,client_ref,provider_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,COALESCE(trade_id,''),quote_id,correlation_id,provider_id,bank_code,account_number,COALESCE(supersedes,'') FROM settlement_instructions WHERE id=$1`, id).Scan(&i.ID, &i.ClientRef, &i.ProviderRef, &i.Beneficiary, &i.AmountMinor, &i.Currency, &i.Purpose, &state, &i.CreatedAt, &i.UpdatedAt, &i.InstitutionID, &i.TradeID, &i.QuoteID, &i.CorrelationID, &i.ProviderID, &i.BankCode, &i.AccountNumber, &i.Supersedes)
	i.State = settlementcore.State(state)
	return i, err
}
