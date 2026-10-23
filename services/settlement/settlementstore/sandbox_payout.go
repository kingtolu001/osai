package settlementstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type SandboxPayoutJob struct {
	SettlementID, BindCommand, FinalCommand       string
	BindAck, SubmitAck, DispatchStarted, FinalAck bool
	ReturnCommand                                 string
	ReturnAck                                     bool
}

func (p Postgres) EnsureSandboxPayoutSchema(ctx context.Context) error {
	_, err := p.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settlement_sandbox_payout_jobs(
 settlement_id text PRIMARY KEY REFERENCES settlement_instructions(id),bind_command text NOT NULL,
 bind_ack boolean NOT NULL DEFAULT false,submit_ack boolean NOT NULL DEFAULT false,
 dispatch_started boolean NOT NULL DEFAULT false,final_command text NOT NULL DEFAULT '',
 final_ack boolean NOT NULL DEFAULT false,observation text NOT NULL DEFAULT '',
 attempted_at timestamptz NOT NULL DEFAULT now());
 ALTER TABLE settlement_sandbox_payout_jobs ADD COLUMN IF NOT EXISTS return_command text NOT NULL DEFAULT '';
 ALTER TABLE settlement_sandbox_payout_jobs ADD COLUMN IF NOT EXISTS return_ack boolean NOT NULL DEFAULT false;
 ALTER TABLE settlement_finality_outbox DROP CONSTRAINT IF EXISTS settlement_finality_outbox_status_check;
 ALTER TABLE settlement_finality_outbox ADD CONSTRAINT settlement_finality_outbox_status_check CHECK(status IN ('CONFIRMED','FAILED','RETURNED','REVERSED','RETRYING'));`)
	return err
}

func (p Postgres) SandboxPayoutJob(ctx context.Context, id string) (SandboxPayoutJob, error) {
	var j SandboxPayoutJob
	err := p.DB.QueryRowContext(ctx, `SELECT settlement_id,bind_command,bind_ack,submit_ack,dispatch_started,final_command,final_ack,return_command,return_ack FROM settlement_sandbox_payout_jobs WHERE settlement_id=$1`, id).Scan(&j.SettlementID, &j.BindCommand, &j.BindAck, &j.SubmitAck, &j.DispatchStarted, &j.FinalCommand, &j.FinalAck, &j.ReturnCommand, &j.ReturnAck)
	return j, err
}

func SandboxPayoutBinding(i settlementcore.Instruction) sandboxaccounting.PayoutBinding {
	return sandboxaccounting.PayoutBinding{SettlementID: i.ID, ClientReference: i.ClientRef, ProviderID: i.ProviderID, BeneficiaryID: i.Beneficiary, Currency: i.Currency, AmountMinor: i.AmountMinor}
}

func SandboxPayoutCommand(i settlementcore.Instruction, terms sandboxaccounting.Terms, event string, revision int64, payload string) sandboxaccounting.Command {
	hash := sha256.Sum256([]byte(payload))
	b := SandboxPayoutBinding(i)
	return sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, ExpectedRevision: revision, EventID: "sbx:payout:" + i.ID + ":" + event, Event: event, Actor: "sbx:settlement", CorrelationID: i.CorrelationID, Reason: "authenticated local payout fixture lifecycle", EvidencePayload: payload, EvidenceHash: hex.EncodeToString(hash[:]), Terms: terms, Payout: &b}
}

func (p Postgres) PrepareSandboxPayout(ctx context.Context, i settlementcore.Instruction, position sandboxaccounting.Position) (SandboxPayoutJob, error) {
	t := position.Terms
	if t.Plan == nil || position.State != "FX_HELD" || (position.Payout != nil && (position.PayoutState != "FAILED" || position.Payout.SettlementID != i.Supersedes)) || t.TradeID != i.TradeID || t.InstitutionID != i.InstitutionID || t.Plan.QuoteID != i.QuoteID || i.Currency != "USD" || t.Plan.DestinationMinor != i.AmountMinor || i.ProviderID != "sbx:provider" || t.Plan.ProviderID != i.ProviderID || t.Plan.BeneficiaryID != i.Beneficiary {
		return SandboxPayoutJob{}, errors.New("verified unbound destination proceeds required")
	}
	payload, _ := json.Marshal(SandboxPayoutBinding(i))
	command := SandboxPayoutCommand(i, t, "payout_bind", position.Revision, string(payload))
	raw, err := json.Marshal(command)
	if err != nil {
		return SandboxPayoutJob{}, err
	}
	if _, err = p.DB.ExecContext(ctx, `INSERT INTO settlement_sandbox_payout_jobs(settlement_id,bind_command) VALUES($1,$2) ON CONFLICT DO NOTHING`, i.ID, string(raw)); err != nil {
		return SandboxPayoutJob{}, err
	}
	return p.SandboxPayoutJob(ctx, i.ID)
}

// Each acknowledgement can be replayed. The dispatch fence, by contrast, has
// exactly one winner and is committed before the first provider POST.
func (p Postgres) AdvanceSandboxPayout(ctx context.Context, id, step string) (bool, error) {
	query := ""
	switch step {
	case "bind":
		query = `UPDATE settlement_sandbox_payout_jobs SET bind_ack=true WHERE settlement_id=$1`
	case "submit":
		query = `UPDATE settlement_sandbox_payout_jobs SET submit_ack=true WHERE settlement_id=$1 AND bind_ack`
	case "dispatch":
		query = `UPDATE settlement_sandbox_payout_jobs SET dispatch_started=true WHERE settlement_id=$1 AND submit_ack AND NOT dispatch_started`
	case "final":
		query = `UPDATE settlement_sandbox_payout_jobs SET final_ack=true WHERE settlement_id=$1 AND final_command<>''`
	case "return":
		query = `UPDATE settlement_sandbox_payout_jobs SET return_ack=true WHERE settlement_id=$1 AND return_command<>''`
	default:
		return false, errors.New("unsupported sandbox payout step")
	}
	result, err := p.DB.ExecContext(ctx, query, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (p Postgres) FreezeSandboxPayoutFinal(ctx context.Context, id string, c sandboxaccounting.Command) (sandboxaccounting.Command, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	_, err = p.DB.ExecContext(ctx, `UPDATE settlement_sandbox_payout_jobs SET final_command=$2,observation=$3 WHERE settlement_id=$1 AND final_command='' AND dispatch_started`, id, string(raw), c.EvidencePayload)
	if err != nil {
		return c, err
	}
	j, err := p.SandboxPayoutJob(ctx, id)
	if err != nil {
		return c, err
	}
	var frozen sandboxaccounting.Command
	if json.Unmarshal([]byte(j.FinalCommand), &frozen) != nil || frozen.Event != c.Event {
		return c, errors.New("conflicting sandbox payout finality")
	}
	return frozen, nil
}

func (p Postgres) PendingSandboxPayouts(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("bounded sandbox payout scan required")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT i.id FROM settlement_instructions i LEFT JOIN settlement_sandbox_payout_jobs j ON j.settlement_id=i.id WHERE i.provider_id='sbx:provider' AND i.state NOT IN ('CONFIRMED','FAILED','RETURNED','REVERSED','MANUAL_REVIEW') ORDER BY COALESCE(j.attempted_at,i.created_at),i.id LIMIT $1`, limit)
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

func (p Postgres) TouchSandboxPayout(ctx context.Context, id string) error {
	_, err := p.DB.ExecContext(ctx, `UPDATE settlement_sandbox_payout_jobs SET attempted_at=now() WHERE settlement_id=$1`, id)
	return err
}
