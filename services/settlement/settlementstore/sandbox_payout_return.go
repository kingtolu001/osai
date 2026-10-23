package settlementstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/services/settlement/settlementcore"
)

func (p Postgres) PendingSandboxPayoutReturns(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("bounded return recovery scan required")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT i.id FROM settlement_instructions i JOIN settlement_sandbox_payout_jobs j ON j.settlement_id=i.id WHERE i.provider_id='sbx:provider' AND i.state IN ('CONFIRMED','RETURNED','REVERSED') AND j.final_ack AND NOT j.return_ack AND j.attempted_at<=now()-interval '5 seconds' ORDER BY j.attempted_at,i.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (p Postgres) FreezeSandboxPayoutReturn(ctx context.Context, id string, c sandboxaccounting.Command) (sandboxaccounting.Command, error) {
	if c.Event != "return" && c.Event != "reversal" {
		return c, errors.New("full returned payout observation required")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	_, err = p.DB.ExecContext(ctx, `UPDATE settlement_sandbox_payout_jobs SET return_command=$2 WHERE settlement_id=$1 AND return_command='' AND final_ack`, id, string(raw))
	if err != nil {
		return c, err
	}
	j, err := p.SandboxPayoutJob(ctx, id)
	if err != nil {
		return c, err
	}
	var frozen sandboxaccounting.Command
	if json.Unmarshal([]byte(j.ReturnCommand), &frozen) != nil || frozen.Event != c.Event {
		return c, errors.New("conflicting returned payout evidence")
	}
	return frozen, nil
}

// RecordSandboxPayoutReturn follows a committed Ledger return receipt. It adds
// new facts and retains the original payout/evidence; no posted history is edited.
func (p Postgres) RecordSandboxPayoutReturn(ctx context.Context, i settlementcore.Instruction, next settlementcore.State, evidence provider.Evidence) error {
	if (next != settlementcore.Returned && next != settlementcore.Reversed) || i.ProviderID != "sbx:provider" || !evidence.IsCompleteFor(i.ClientRef) {
		return errors.New("verified sandbox return required")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, reference, institution, trade, correlation string
	if err = tx.QueryRowContext(ctx, `SELECT state,provider_ref,institution_id,trade_id,correlation_id FROM settlement_instructions WHERE id=$1 FOR UPDATE`, i.ID).Scan(&state, &reference, &institution, &trade, &correlation); err != nil {
		return err
	}
	if reference != i.ProviderRef || institution != i.InstitutionID || trade != i.TradeID || correlation != i.CorrelationID {
		return errors.New("return instruction identity changed")
	}
	if state == string(next) {
		return tx.Commit()
	}
	if state != string(settlementcore.Confirmed) {
		return errors.New("only a confirmed payout can be returned")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_instructions SET state=$2,updated_at=now() WHERE id=$1`, i.ID, next); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_transitions(settlement_id,from_state,to_state,event,reason,source,occurred_at) VALUES($1,'CONFIRMED',$2,'verified_payout_return','authenticated full destination return','provider',now())`, i.ID, next); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_evidence(evidence_id,settlement_id,provider,method,request_ref,raw_payload_hash,parsed_at,correlation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(evidence_id) DO NOTHING`, evidence.ID, i.ID, evidence.Provider, evidence.Method, evidence.RequestRef, evidence.RawPayloadHash, evidence.ParsedAt, evidence.CorrelationID); err != nil {
		return err
	}
	eventType := "trade.returned"
	if next == settlementcore.Reversed {
		eventType = "trade.reversed"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_domain_outbox(event_id,institution_id,settlement_id,trade_id,event_type,correlation_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,now()) ON CONFLICT(event_id) DO NOTHING`, "sandbox_return:"+i.ID, institution, i.ID, trade, eventType, correlation); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_finality_outbox(event_id,institution_id,settlement_id,trade_id,status,correlation_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,now()) ON CONFLICT(event_id) DO NOTHING`, "sandbox_return_finality:"+i.ID, institution, i.ID, trade, next, correlation); err != nil {
		return err
	}
	return tx.Commit()
}
