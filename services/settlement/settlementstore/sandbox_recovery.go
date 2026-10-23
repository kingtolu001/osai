package settlementstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/ids"
	"github.com/osai/osai/pkg/sandboxaccounting"
)

type SandboxRecovery struct {
	ID, InstitutionID, SettlementID, Maker, Checker, Reason, State, NextSettlementID string
	Command                                                                          sandboxaccounting.Command
}

func (p Postgres) EnsureSandboxRecoverySchema(ctx context.Context) error {
	_, err := p.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settlement_sandbox_recovery(
 id text PRIMARY KEY,institution_id text NOT NULL,settlement_id text NOT NULL REFERENCES settlement_instructions(id),
 trade_id text NOT NULL,revision bigint NOT NULL,maker text NOT NULL,checker text NOT NULL DEFAULT '',reason text NOT NULL,
 state text NOT NULL CHECK(state IN ('PROPOSED','APPROVED','APPLIED')),command_json text NOT NULL,
 next_settlement_id text NOT NULL DEFAULT '',created_at timestamptz NOT NULL DEFAULT now(),attempted_at timestamptz NOT NULL DEFAULT now(),applied_at timestamptz,
 UNIQUE(trade_id,revision));`)
	return err
}

func (p Postgres) SandboxRecovery(ctx context.Context, id, institution string) (SandboxRecovery, error) {
	var v SandboxRecovery
	var raw string
	err := p.DB.QueryRowContext(ctx, `SELECT id,institution_id,settlement_id,maker,checker,reason,state,command_json,next_settlement_id FROM settlement_sandbox_recovery WHERE id=$1 AND institution_id=$2`, id, institution).Scan(&v.ID, &v.InstitutionID, &v.SettlementID, &v.Maker, &v.Checker, &v.Reason, &v.State, &raw, &v.NextSettlementID)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &v.Command)
	}
	return v, err
}

func (p Postgres) ProposeSandboxRecovery(ctx context.Context, v SandboxRecovery) error {
	raw, err := json.Marshal(v.Command)
	if err != nil {
		return err
	}
	_, err = p.DB.ExecContext(ctx, `INSERT INTO settlement_sandbox_recovery(id,institution_id,settlement_id,trade_id,revision,maker,reason,state,command_json) VALUES($1,$2,$3,$4,$5,$6,$7,'PROPOSED',$8) ON CONFLICT(id) DO NOTHING`, v.ID, v.InstitutionID, v.SettlementID, v.Command.Terms.TradeID, v.Command.ExpectedRevision, v.Maker, v.Reason, string(raw))
	if err != nil {
		return err
	}
	prior, err := p.SandboxRecovery(ctx, v.ID, v.InstitutionID)
	if err != nil {
		return err
	}
	// Approval freezes the checker fields; proposal retries still compare all maker inputs.
	if prior.SettlementID != v.SettlementID || prior.Maker != v.Maker || prior.Reason != v.Reason || prior.Command.Event != v.Command.Event || prior.Command.ExpectedRevision != v.Command.ExpectedRevision {
		return errors.New("recovery proposal conflict")
	}
	return nil
}

func (p Postgres) ApproveSandboxRecovery(ctx context.Context, id, institution, checker string, command sandboxaccounting.Command) error {
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	r, err := p.DB.ExecContext(ctx, `UPDATE settlement_sandbox_recovery SET checker=$3,command_json=$4,state='APPROVED' WHERE id=$1 AND institution_id=$2 AND state='PROPOSED' AND maker<>$3`, id, institution, checker, string(raw))
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	v, err := p.SandboxRecovery(ctx, id, institution)
	if err != nil {
		return err
	}
	stored, _ := json.Marshal(v.Command)
	if (v.State == "APPROVED" || v.State == "APPLIED") && v.Checker == checker && string(stored) == string(raw) {
		return nil
	}
	return errors.New("distinct authenticated checker required")
}

func (p Postgres) PendingSandboxRecoveries(ctx context.Context) ([]SandboxRecovery, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT id,institution_id FROM settlement_sandbox_recovery WHERE state='APPROVED' ORDER BY attempted_at,id LIMIT 25`)
	if err != nil {
		return nil, err
	}
	type key struct{ id, inst string }
	var keys []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.inst); err != nil {
			break
		}
		keys = append(keys, k)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []SandboxRecovery
	for _, k := range keys {
		v, e := p.SandboxRecovery(ctx, k.id, k.inst)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, nil
}

// Complete is called only after the immutable approved Ledger command receipt.
// Retry creates a distinct successor and its ordered Trade transition atomically.
func (p Postgres) CompleteSandboxRecovery(ctx context.Context, v SandboxRecovery) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, raw string
	if err = tx.QueryRowContext(ctx, `SELECT state,command_json FROM settlement_sandbox_recovery WHERE id=$1 FOR UPDATE`, v.ID).Scan(&state, &raw); err != nil {
		return err
	}
	want, _ := json.Marshal(v.Command)
	if raw != string(want) {
		return errors.New("approved recovery changed")
	}
	if state == "APPLIED" {
		return tx.Commit()
	}
	if state != "APPROVED" {
		return errors.New("approved recovery required")
	}
	nextID := ""
	if v.Command.Event == "retry_payout" {
		nextID = ids.NewSettlementID()
		ref := ids.NewSettlementID()
		r, e := tx.ExecContext(ctx, `INSERT INTO settlement_instructions(id,client_ref,beneficiary,amount_minor,currency,purpose,state,supersedes,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number)
 SELECT $2,$3,beneficiary,amount_minor,currency,purpose,'CREATED',id,clock_timestamp(),clock_timestamp(),institution_id,trade_id,quote_id,correlation_id,provider_id,bank_code,account_number FROM settlement_instructions WHERE id=$1 AND state='FAILED' AND provider_id='sbx:provider' AND institution_id=$4`, v.SettlementID, nextID, ref, v.InstitutionID)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return errors.New("failed original attempt required")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_finality_outbox(event_id,institution_id,settlement_id,trade_id,status,correlation_id,occurred_at,supersedes) VALUES($1,$2,$3,$4,'RETRYING',$5,clock_timestamp(),$6)`, "sbx:retry:"+v.ID, v.InstitutionID, nextID, v.Command.Terms.TradeID, v.Command.CorrelationID, v.SettlementID)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_sandbox_recovery SET state='APPLIED',next_settlement_id=$2,applied_at=clock_timestamp() WHERE id=$1`, v.ID, nextID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
