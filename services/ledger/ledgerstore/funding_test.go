package ledgerstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/services/ledger/ledgerapi"
)

func TestFundingReservationsCannotOverdrawAndFinalityRequiresHold(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL DSN required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	store := Postgres{DB: db, CustodyBankID: "test_custody"}
	service, err := ledgerapi.NewServiceWithStorage(store)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	inst := "inst_funding_" + suffix
	deposit := ledgerapi.PrefundCommand{InstitutionID: inst, Currency: "NGN", AmountMinor: 100, EvidenceReference: "bank_funding_" + suffix}
	first, replay, err := service.PostLocalPrefund(context.Background(), deposit)
	if err != nil || replay || first == "" {
		t.Fatalf("prefund post: journal=%s replay=%v err=%v", first, replay, err)
	}
	second, replay, err := service.PostLocalPrefund(context.Background(), deposit)
	if err != nil || !replay || second != first {
		t.Fatalf("prefund replay: journal=%s replay=%v err=%v", second, replay, err)
	}
	commands := []ledgerapi.FundingCommand{
		{InstitutionID: inst, TradeID: "trd_a_" + suffix, Currency: "NGN", AmountMinor: 100},
		{InstitutionID: inst, TradeID: "trd_b_" + suffix, Currency: "NGN", AmountMinor: 100},
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, cmd := range commands {
		wg.Add(1)
		go func(cmd ledgerapi.FundingCommand) {
			defer wg.Done()
			_, _, err := service.ReserveFunding(context.Background(), cmd)
			results <- err
		}(cmd)
	}
	wg.Wait()
	close(results)
	successes, insufficient := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ledgerapi.ErrInsufficientFunding) {
			insufficient++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("concurrent reservations: success=%d insufficient=%d", successes, insufficient)
	}
	if count := proofFundingCount(t, db, `SELECT count(*) FROM ledger_funding_reservations WHERE institution_id=$1`, inst); count != 1 {
		t.Fatalf("expected one hold, got %d", count)
	}
	unfunded := ledgerapi.Command{InstitutionID: inst, TradeID: "trd_unfunded_" + suffix, SettlementID: "si_unfunded_" + suffix, ExternalReference: "ref_unfunded_" + suffix, AmountMinor: 100, Currency: "NGN"}
	if _, _, err := service.ConfirmCommand(unfunded); err == nil {
		t.Fatal("unfunded settlement journal accepted")
	}

	fxInst := "inst_fxguard_" + suffix
	fxTrade := "trd_fxguard_" + suffix
	fxDeposit := ledgerapi.PrefundCommand{InstitutionID: fxInst, Currency: "NGN", AmountMinor: 100, EvidenceReference: "bank_fxguard_" + suffix}
	if _, _, err = service.PostLocalPrefund(context.Background(), fxDeposit); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.ReserveFunding(context.Background(), ledgerapi.FundingCommand{InstitutionID: fxInst, TradeID: fxTrade, Currency: "NGN", AmountMinor: 100}); err != nil {
		t.Fatal(err)
	}
	crossCurrency := ledgerapi.Command{InstitutionID: fxInst, TradeID: fxTrade, SettlementID: "si_fxguard_" + suffix, ExternalReference: "ref_fxguard_" + suffix, AmountMinor: 100, Currency: "USD"}
	if _, _, err = service.ConfirmCommand(crossCurrency); !errors.Is(err, ledgerapi.ErrFundingMismatch) {
		t.Fatalf("cross-currency confirmation should fail closed, got %v", err)
	}
	if _, _, err = service.FailCommand(crossCurrency); !errors.Is(err, ledgerapi.ErrFundingMismatch) {
		t.Fatalf("cross-currency failure release should fail closed, got %v", err)
	}
	reservation, err := service.GetFundingReservation(context.Background(), ledgerapi.FundingCommand{InstitutionID: fxInst, TradeID: fxTrade, Currency: "NGN", AmountMinor: 100})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.State != "ACTIVE" {
		t.Fatalf("cross-currency finality changed source reservation to %s", reservation.State)
	}
	if count := proofFundingCount(t, db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1 AND kind IN ('settlement.confirmed','settlement.failed')`, fxTrade); count != 0 {
		t.Fatalf("cross-currency finality posted %d settlement journals", count)
	}

	feeInst := "inst_fee_hold_" + suffix
	feeTrade := "trd_fee_hold_" + suffix
	feeDeposit := ledgerapi.PrefundCommand{InstitutionID: feeInst, Currency: "NGN", AmountMinor: 20_000, EvidenceReference: "bank_fee_hold_" + suffix}
	if _, _, err = service.PostLocalPrefund(context.Background(), feeDeposit); err != nil {
		t.Fatal(err)
	}
	snapshot := fundingPricingSnapshot(t)
	feeCommand := ledgerapi.FundingCommand{
		InstitutionID: feeInst, TradeID: feeTrade, Currency: "NGN", AmountMinor: 20_000,
		SourcePrincipalMinor: 19_500, FeeMinor: 500, PricingHash: snapshot.Hash, PricingSnapshot: &snapshot,
	}
	var feeWG sync.WaitGroup
	feeResults := make(chan struct {
		replay bool
		err    error
	}, 2)
	for range 2 {
		feeWG.Add(1)
		go func() {
			defer feeWG.Done()
			_, replay, reserveErr := service.ReserveFunding(context.Background(), feeCommand)
			feeResults <- struct {
				replay bool
				err    error
			}{replay: replay, err: reserveErr}
		}()
	}
	feeWG.Wait()
	close(feeResults)
	feeReplays, feeSuccesses := 0, 0
	for result := range feeResults {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.replay {
			feeReplays++
		} else {
			feeSuccesses++
		}
	}
	if feeSuccesses != 1 || feeReplays != 1 {
		t.Fatalf("concurrent fee reservation was not idempotent: new=%d replay=%d", feeSuccesses, feeReplays)
	}
	conflictingFee := feeCommand
	alteredSnapshot := snapshot
	alteredSnapshot.ExpiresAt = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	alteredSnapshot.Hash, err = pricing.Hash(alteredSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	conflictingFee.PricingSnapshot = &alteredSnapshot
	conflictingFee.PricingHash = alteredSnapshot.Hash
	if _, _, err = service.ReserveFunding(context.Background(), conflictingFee); !errors.Is(err, ledgerapi.ErrIdempotencyConflict) {
		t.Fatalf("different fee snapshot reused a trade reservation: %v", err)
	}
	var storedPrincipal, storedFee, heldAmount int64
	if err = db.QueryRow(`SELECT source_principal_minor,fee_minor,amount_minor FROM ledger_funding_reservations WHERE trade_id=$1`, feeTrade).Scan(&storedPrincipal, &storedFee, &heldAmount); err != nil {
		t.Fatal(err)
	}
	if storedPrincipal != 19_500 || storedFee != 500 || heldAmount != 20_000 ||
		proofFundingCount(t, db, `SELECT count(*) FROM ledger_journals WHERE dedup_key=$1`, "hold:"+feeTrade) != 1 {
		t.Fatalf("persisted fee reservation economics differ: principal=%d fee=%d debit=%d", storedPrincipal, storedFee, heldAmount)
	}
	var storedSnapshotJSON string
	if err = db.QueryRow(`SELECT pricing_snapshot_json FROM ledger_funding_reservations WHERE trade_id=$1`, feeTrade).Scan(&storedSnapshotJSON); err != nil {
		t.Fatal(err)
	}
	var storedSnapshot pricing.Snapshot
	if err = json.Unmarshal([]byte(storedSnapshotJSON), &storedSnapshot); err != nil {
		t.Fatal(err)
	}
	if storedSnapshot != snapshot {
		t.Fatalf("stored Ledger pricing snapshot differs: got=%+v want=%+v", storedSnapshot, snapshot)
	}
	reservation, err = service.GetFundingReservation(context.Background(), feeCommand)
	if err != nil || reservation.PricingSnapshot == nil || *reservation.PricingSnapshot != snapshot {
		t.Fatalf("Ledger pricing snapshot query failed: reservation=%+v err=%v", reservation, err)
	}
	if _, err = db.Exec(`UPDATE ledger_funding_reservations SET pricing_snapshot_json='{}' WHERE trade_id=$1`, feeTrade); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetFundingReservation(context.Background(), feeCommand); err == nil {
		t.Fatal("tampered persisted pricing snapshot was returned as a valid reservation")
	}
}

func fundingPricingSnapshot(t *testing.T) pricing.Snapshot {
	t.Helper()
	snapshot := pricing.Snapshot{
		Mode: "FIXED_SOURCE", RequestedAmountMinor: 20_000, SourcePrincipalMinor: 19_500,
		FeeMinor: 500, TotalDebitMinor: 20_000, DestinationAmountMinor: 19_500,
		SourceCurrency: "NGN", DestinationCurrency: "NGN", RateScaled: pricing.RateScale,
		RateDirection: "NGN_PER_NGN", RateScale: pricing.RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fee_test", FeeScheduleVersion: 1,
		FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500,
		MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	hash, err := pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Hash = hash
	if err = pricing.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func proofFundingCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
