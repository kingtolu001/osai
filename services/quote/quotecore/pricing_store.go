package quotecore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/postgres"
)

var (
	ErrPricingApprovalConflict = errors.New("pricing proposal is not eligible for this action")
	ErrPricingMakerChecker     = errors.New("pricing proposal requires a different checker")
)

type PricingAction struct {
	ProposalID string
	Action     string
	ActorID    string
	Reason     string
	OccurredAt time.Time
}

func ensurePricingSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS quote_fee_schedule_proposals (
		id text PRIMARY KEY,
		institution_id text NOT NULL,
		source_currency text NOT NULL,
		destination_currency text NOT NULL,
		version bigint NOT NULL CHECK (version > 0),
		body jsonb NOT NULL,
		maker_actor_id text NOT NULL,
		maker_reason text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
		UNIQUE(institution_id, source_currency, destination_currency, version)
	);
	CREATE TABLE IF NOT EXISTS quote_fee_schedule_actions (
		id bigserial PRIMARY KEY,
		proposal_id text NOT NULL REFERENCES quote_fee_schedule_proposals(id),
		action text NOT NULL CHECK (action IN ('PROPOSE','APPROVE','REJECT')),
		actor_id text NOT NULL,
		reason text NOT NULL,
		occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
	);
	CREATE TABLE IF NOT EXISTS quote_active_fee_schedules (
		institution_id text NOT NULL,
		source_currency text NOT NULL,
		destination_currency text NOT NULL,
		proposal_id text NOT NULL REFERENCES quote_fee_schedule_proposals(id),
		PRIMARY KEY(institution_id, source_currency, destination_currency)
	);
	CREATE OR REPLACE FUNCTION reject_quote_pricing_audit_mutation() RETURNS trigger
	LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'quote pricing audit is append only'; END $$;
	DROP TRIGGER IF EXISTS quote_fee_schedule_actions_immutable ON quote_fee_schedule_actions;
	CREATE TRIGGER quote_fee_schedule_actions_immutable BEFORE UPDATE OR DELETE ON quote_fee_schedule_actions
	FOR EACH ROW EXECUTE FUNCTION reject_quote_pricing_audit_mutation();
	DROP TRIGGER IF EXISTS quote_fee_schedule_proposals_immutable ON quote_fee_schedule_proposals;
	CREATE TRIGGER quote_fee_schedule_proposals_immutable BEFORE UPDATE OR DELETE ON quote_fee_schedule_proposals
	FOR EACH ROW EXECUTE FUNCTION reject_quote_pricing_audit_mutation();`)
	return err
}

func (s *QuoteStore) ProposeFeeSchedule(ctx context.Context, policy FeeSchedule, makerActorID, reason string) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("durable quote pricing store required")
	}
	makerActorID, reason = strings.TrimSpace(makerActorID), strings.TrimSpace(reason)
	if makerActorID == "" || reason == "" || len(reason) > 500 {
		return "", fmt.Errorf("%w: maker and bounded reason are required", ErrInvalidPricingPolicy)
	}
	policy.ID = "fee_" + uuid.NewString()
	policy.Version = 1
	if err := policy.Validate(); err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	pairKey := fmt.Sprintf("fee-schedule:%s:%s:%s", policy.InstitutionID, policy.SourceCurrency, policy.DestinationCurrency)
	if err = postgres.Lock(tx, pairKey); err != nil {
		return "", err
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM quote_fee_schedule_proposals
		WHERE institution_id=$1 AND source_currency=$2 AND destination_currency=$3`,
		policy.InstitutionID, policy.SourceCurrency, policy.DestinationCurrency).Scan(&version); err != nil {
		return "", err
	}
	policy.Version = version
	body, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO quote_fee_schedule_proposals
		(id,institution_id,source_currency,destination_currency,version,body,maker_actor_id,maker_reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		policy.ID, policy.InstitutionID, policy.SourceCurrency, policy.DestinationCurrency,
		policy.Version, string(body), makerActorID, reason); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO quote_fee_schedule_actions(proposal_id,action,actor_id,reason)
		VALUES($1,'PROPOSE',$2,$3)`, policy.ID, makerActorID, reason); err != nil {
		return "", err
	}
	return policy.ID, tx.Commit()
}

func (s *QuoteStore) ApproveFeeSchedule(ctx context.Context, proposalID, institutionID, checkerActorID, reason string) error {
	return s.applyFeeScheduleDecision(ctx, proposalID, institutionID, checkerActorID, reason, "APPROVE")
}

func (s *QuoteStore) RejectFeeSchedule(ctx context.Context, proposalID, institutionID, checkerActorID, reason string) error {
	return s.applyFeeScheduleDecision(ctx, proposalID, institutionID, checkerActorID, reason, "REJECT")
}

func (s *QuoteStore) applyFeeScheduleDecision(ctx context.Context, proposalID, institutionID, checkerActorID, reason, action string) error {
	if s == nil || s.db == nil {
		return errors.New("durable quote pricing store required")
	}
	proposalID, checkerActorID, reason = strings.TrimSpace(proposalID), strings.TrimSpace(checkerActorID), strings.TrimSpace(reason)
	if proposalID == "" || checkerActorID == "" || reason == "" || len(reason) > 500 {
		return fmt.Errorf("%w: checker and bounded reason are required", ErrInvalidPricingPolicy)
	}
	if institutionID == "" {
		return fmt.Errorf("%w: institution is required", ErrInvalidPricingPolicy)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var makerActorID, proposalInstitutionID, sourceCurrency, destinationCurrency string
	var raw []byte
	query := `SELECT maker_actor_id,institution_id,source_currency,destination_currency,body
		FROM quote_fee_schedule_proposals WHERE id=$1 AND institution_id=$2 FOR UPDATE`
	args := []any{proposalID, institutionID}
	if err = tx.QueryRowContext(ctx, query, args...).
		Scan(&makerActorID, &proposalInstitutionID, &sourceCurrency, &destinationCurrency, &raw); err != nil {
		return err
	}
	if checkerActorID == makerActorID {
		return ErrPricingMakerChecker
	}
	pairKey := fmt.Sprintf("fee-schedule:%s:%s:%s", proposalInstitutionID, sourceCurrency, destinationCurrency)
	if err = postgres.Lock(tx, pairKey); err != nil {
		return err
	}
	var latestAction string
	err = tx.QueryRowContext(ctx, `SELECT action FROM quote_fee_schedule_actions
		WHERE proposal_id=$1 ORDER BY id DESC LIMIT 1`, proposalID).Scan(&latestAction)
	if err != nil {
		return err
	}
	if latestAction != "PROPOSE" {
		return ErrPricingApprovalConflict
	}
	var policy FeeSchedule
	if err = json.Unmarshal(raw, &policy); err != nil {
		return err
	}
	if action == "APPROVE" {
		var activeVersion int64
		err = tx.QueryRowContext(ctx, `SELECT p.version FROM quote_active_fee_schedules a
			JOIN quote_fee_schedule_proposals p ON p.id=a.proposal_id
			WHERE a.institution_id=$1 AND a.source_currency=$2 AND a.destination_currency=$3`,
			proposalInstitutionID, sourceCurrency, destinationCurrency).Scan(&activeVersion)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && policy.Version <= activeVersion {
			return ErrPricingApprovalConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO quote_fee_schedule_actions(proposal_id,action,actor_id,reason)
		VALUES($1,$2,$3,$4)`, proposalID, action, checkerActorID, reason); err != nil {
		return err
	}
	if action == "APPROVE" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO quote_active_fee_schedules
			(institution_id,source_currency,destination_currency,proposal_id) VALUES($1,$2,$3,$4)
			ON CONFLICT(institution_id,source_currency,destination_currency)
			DO UPDATE SET proposal_id=excluded.proposal_id`,
			proposalInstitutionID, sourceCurrency, destinationCurrency, proposalID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *QuoteStore) GetFeeSchedule(ctx context.Context, institutionID, sourceCurrency, destinationCurrency string) (FeeSchedule, error) {
	if s == nil || s.db == nil {
		return FeeSchedule{}, ErrPricingPolicyUnavailable
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT p.body FROM quote_active_fee_schedules a
		JOIN quote_fee_schedule_proposals p ON p.id=a.proposal_id
		WHERE a.institution_id=$1 AND a.source_currency=$2 AND a.destination_currency=$3`,
		institutionID, sourceCurrency, destinationCurrency).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return FeeSchedule{}, ErrPricingPolicyUnavailable
	}
	if err != nil {
		return FeeSchedule{}, err
	}
	var policy FeeSchedule
	if err = json.Unmarshal(raw, &policy); err != nil {
		return FeeSchedule{}, err
	}
	if err = policy.Validate(); err != nil {
		return FeeSchedule{}, err
	}
	return policy, nil
}

func (s *QuoteStore) ListPricingActions(ctx context.Context, proposalID string) ([]PricingAction, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("durable quote pricing store required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT proposal_id,action,actor_id,reason,occurred_at
		FROM quote_fee_schedule_actions WHERE proposal_id=$1 ORDER BY id`, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	actions := make([]PricingAction, 0)
	for rows.Next() {
		var action PricingAction
		if err := rows.Scan(&action.ProposalID, &action.Action, &action.ActorID, &action.Reason, &action.OccurredAt); err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return actions, nil
}
