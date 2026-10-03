package settlementstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/osai/osai/pkg/ids"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type Postgres struct{ DB *sql.DB }

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
	_, err := p.DB.ExecContext(ctx, `INSERT INTO settlement_instructions(id,client_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number) VALUES($1,$2,$3,$4,$5,$6,'CREATED',$7,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (trade_id) WHERE trade_id IS NOT NULL DO NOTHING`, id, ref, beneficiary, amountMinor, currency, purpose, now, institutionID, tradeID, quoteID, correlationID, providerID, bankCode, accountNumber)
	if err != nil {
		return settlementcore.Instruction{}, err
	}
	var i settlementcore.Instruction
	var state string
	err = p.DB.QueryRowContext(ctx, `SELECT id,client_ref,provider_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions WHERE trade_id=$1`, tradeID).Scan(&i.ID, &i.ClientRef, &i.ProviderRef, &i.Beneficiary, &i.AmountMinor, &i.Currency, &i.Purpose, &state, &i.CreatedAt, &i.UpdatedAt, &i.InstitutionID, &i.TradeID, &i.QuoteID, &i.CorrelationID, &i.ProviderID, &i.BankCode, &i.AccountNumber)
	if err != nil {
		return settlementcore.Instruction{}, err
	}
	i.State = settlementcore.State(state)
	if i.InstitutionID != institutionID || i.QuoteID != quoteID || i.AmountMinor != amountMinor || i.Currency != currency || i.ProviderID != providerID || i.BankCode != bankCode || i.AccountNumber != accountNumber {
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
	var current string
	var existingRef string
	if err = tx.QueryRowContext(ctx, `SELECT state,provider_ref FROM settlement_instructions WHERE id=$1 FOR UPDATE`, id).Scan(&current, &existingRef); err != nil {
		return err
	}
	if current == string(settlementcore.Confirmed) || current == string(settlementcore.Failed) {
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
			if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_transitions(settlement_id,from_state,to_state,event,reason,source,occurred_at) VALUES($1,$2,$3,$4,$5,'provider',now())`, id, current, next, "provider_runtime", "provider status evidence"); err != nil {
				return err
			}
		}
	}
	if evidence.ID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_evidence(evidence_id,settlement_id,provider,method,request_ref,raw_payload_hash,parsed_at,correlation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(evidence_id) DO NOTHING`, evidence.ID, id, evidence.Provider, evidence.Method, evidence.RequestRef, evidence.RawPayloadHash, evidence.ParsedAt, evidence.CorrelationID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p Postgres) GetByID(ctx context.Context, id string) (settlementcore.Instruction, error) {
	var i settlementcore.Instruction
	var state string
	err := p.DB.QueryRowContext(ctx, `SELECT id,client_ref,provider_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,COALESCE(trade_id,''),quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions WHERE id=$1`, id).Scan(&i.ID, &i.ClientRef, &i.ProviderRef, &i.Beneficiary, &i.AmountMinor, &i.Currency, &i.Purpose, &state, &i.CreatedAt, &i.UpdatedAt, &i.InstitutionID, &i.TradeID, &i.QuoteID, &i.CorrelationID, &i.ProviderID, &i.BankCode, &i.AccountNumber)
	i.State = settlementcore.State(state)
	return i, err
}
