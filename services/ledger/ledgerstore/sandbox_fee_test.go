package ledgerstore

import (
	"context"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"testing"
)

func TestSandboxFeeApplicationFinalityIsAtomicAndRestartSafe(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		name := "failure"
		if confirmed {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := sandboxAccountingDB(t)
			ctx := context.Background()
			storage, err := (Postgres{DB: db, CustodyBankID: "local_test_bank"}).WithSandboxFees(ctx)
			if err != nil {
				t.Fatal(err)
			}
			service, err := ledgerapi.NewServiceWithStorage(storage)
			if err != nil {
				t.Fatal(err)
			}
			inst, trade := "sbx:fee_application", "sbx:fee_trade"
			if _, _, err = service.PostLocalPrefund(ctx, ledgerapi.PrefundCommand{InstitutionID: inst, Currency: "NGN", AmountMinor: 20000, EvidenceReference: "sbx:funding"}); err != nil {
				t.Fatal(err)
			}
			snapshot := fundingPricingSnapshot(t)
			funding := ledgerapi.FundingCommand{InstitutionID: inst, TradeID: trade, Currency: "NGN", AmountMinor: 20000, SourcePrincipalMinor: 19500, FeeMinor: 500, PricingHash: snapshot.Hash, PricingSnapshot: &snapshot}
			if _, _, err = service.ReserveFunding(ctx, funding); err != nil {
				t.Fatal(err)
			}
			verify := ledgerapi.FundingCommand{InstitutionID: inst, TradeID: trade, Currency: "NGN", AmountMinor: 19500}
			if _, err = service.GetFundingReservation(ctx, verify); err != nil {
				t.Fatal("principal-only payout verification rejected", err)
			}
			command := ledgerapi.Command{InstitutionID: inst, TradeID: trade, SettlementID: "sbx:settlement", ExternalReference: "sbx:terminal", ProviderReference: "sbx:provider", Beneficiary: "sbx:beneficiary", AmountMinor: 19500, Currency: "NGN", IdempotencyKey: "sbx:finality", CorrelationID: "sbx:correlation", Purpose: "verified fixture finality"}
			wrong := command
			wrong.AmountMinor = 20000
			if _, _, err = storage.FinalizeFunding(ctx, wrong, confirmed); err == nil {
				t.Fatal("payout included service fee")
			}
			disabled := Postgres{DB: db, CustodyBankID: "local_test_bank"}
			if _, _, err = disabled.FinalizeFunding(ctx, command, confirmed); err == nil {
				t.Fatal("fee posting accepted without explicit policy")
			}
			if confirmed {
				if _, err = db.Exec(`CREATE FUNCTION reject_fee_entry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_name='SANDBOX_SERVICE_FEE' THEN RAISE EXCEPTION 'injected fee failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_fee BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_fee_entry()`); err != nil {
					t.Fatal(err)
				}
				if _, _, err = storage.FinalizeFunding(ctx, command, true); err == nil {
					t.Fatal("partial fee commit accepted")
				}
				if proofFundingCount(t, db, `SELECT count(*) FROM ledger_journals WHERE dedup_key=$1`, "settlement:"+trade) != 0 {
					t.Fatal("principal committed without fee")
				}
				if _, err = db.Exec(`DROP TRIGGER reject_fee ON ledger_entries`); err != nil {
					t.Fatal(err)
				}
			}
			journal, replay, err := storage.FinalizeFunding(ctx, command, confirmed)
			if err != nil || replay {
				t.Fatal("terminal posting failed", err)
			}
			restarted, err := (Postgres{DB: db, CustodyBankID: "local_test_bank"}).WithSandboxFees(ctx)
			if err != nil {
				t.Fatal(err)
			}
			again, replay, err := restarted.FinalizeFunding(ctx, command, confirmed)
			if err != nil || !replay || again != journal {
				t.Fatal("restart replay not idempotent", err)
			}
			if confirmed {
				changed := command
				changed.ProviderReference = "sbx:different-provider-reference"
				if _, _, err = restarted.FinalizeFunding(ctx, changed, true); err == nil {
					t.Fatal("replay rebound to different provider evidence")
				}
			}
			var available, held, fee int64
			if err = db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN account_code='2000' THEN credit-debit ELSE 0 END),0),COALESCE(SUM(CASE WHEN account_code='2100' THEN credit-debit ELSE 0 END),0),COALESCE(SUM(CASE WHEN account_name='SANDBOX_SERVICE_FEE' THEN credit-debit ELSE 0 END),0) FROM ledger_entries`).Scan(&available, &held, &fee); err != nil {
				t.Fatal(err)
			}
			if held != 0 || (confirmed && (available != 0 || fee != 500)) || (!confirmed && (available != 20000 || fee != 0)) {
				t.Fatalf("wrong fee outcome available=%d held=%d fee=%d", available, held, fee)
			}
			if proofFundingCount(t, db, `SELECT count(*) FROM ledger_fee_finality WHERE trade_id=$1`, trade) != 1 {
				t.Fatal("finality audit receipt absent or duplicated")
			}
		})
	}
}
