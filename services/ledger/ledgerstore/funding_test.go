package ledgerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
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
}

func proofFundingCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
