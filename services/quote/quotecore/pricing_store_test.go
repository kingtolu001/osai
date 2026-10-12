package quotecore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresFeeScheduleMakerCheckerAndIsolation(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	institutionID := "inst_pricing_" + uuid.NewString()
	policy := testFeeSchedule()
	policy.ID = ""
	policy.Version = 0
	policy.InstitutionID = institutionID

	if _, err = store.GetFeeSchedule(ctx, institutionID, "NGN", "USD"); !errors.Is(err, ErrPricingPolicyUnavailable) {
		t.Fatalf("missing schedule must fail closed, got %v", err)
	}
	proposalID, err := store.ProposeFeeSchedule(ctx, policy, "maker-1", "launch corridor schedule")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ApproveFeeSchedule(ctx, proposalID, institutionID, "maker-1", "self approval"); !errors.Is(err, ErrPricingMakerChecker) {
		t.Fatalf("maker self-approval should fail, got %v", err)
	}
	if _, err = store.GetFeeSchedule(ctx, institutionID, "NGN", "USD"); !errors.Is(err, ErrPricingPolicyUnavailable) {
		t.Fatalf("proposed schedule must not be active, got %v", err)
	}
	if err = store.ApproveFeeSchedule(ctx, proposalID, institutionID, "checker-1", "reviewed"); err != nil {
		t.Fatal(err)
	}
	active, err := store.GetFeeSchedule(ctx, institutionID, "NGN", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != proposalID || active.Version != 1 || active.InstitutionID != institutionID {
		t.Fatalf("unexpected active schedule: %+v", active)
	}
	if _, err = store.GetFeeSchedule(ctx, "another-institution", "NGN", "USD"); !errors.Is(err, ErrPricingPolicyUnavailable) {
		t.Fatalf("schedule crossed institution boundary: %v", err)
	}

	second := policy
	second.FixedFeeMinor = 20
	secondID, err := store.ProposeFeeSchedule(ctx, second, "maker-2", "updated fee")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ApproveFeeSchedule(ctx, secondID, institutionID, "checker-2", "approved update"); err != nil {
		t.Fatal(err)
	}
	if err = store.ApproveFeeSchedule(ctx, proposalID, institutionID, "checker-3", "stale approval"); !errors.Is(err, ErrPricingApprovalConflict) {
		t.Fatalf("older schedule must not replace newer active version, got %v", err)
	}
	active, err = store.GetFeeSchedule(ctx, institutionID, "NGN", "USD")
	if err != nil || active.ID != secondID || active.Version != 2 || active.FixedFeeMinor != 20 {
		t.Fatalf("unexpected replacement schedule: %+v err=%v", active, err)
	}

	actions, err := store.ListPricingActions(ctx, proposalID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].Action != "PROPOSE" || actions[1].Action != "APPROVE" ||
		actions[0].ActorID == actions[1].ActorID {
		t.Fatalf("maker-checker audit incomplete: %+v", actions)
	}
	if _, err = db.ExecContext(ctx, `UPDATE quote_fee_schedule_actions SET reason='changed' WHERE proposal_id=$1`, proposalID); err == nil {
		t.Fatal("pricing audit must be append-only")
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM quote_fee_schedule_proposals WHERE id=$1`, proposalID); err == nil {
		t.Fatal("approved pricing proposal must be immutable")
	}

	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := restarted.GetFeeSchedule(ctx, institutionID, "NGN", "USD")
	if err != nil || afterRestart.ID != secondID {
		t.Fatalf("approved schedule lost after restart: %+v err=%v", afterRestart, err)
	}
}
