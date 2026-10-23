package tradecore

import (
	"database/sql"
	"github.com/google/uuid"
	"os"
	"testing"
)

func TestAuthorizedPayoutRetryFinalityPreservesEarlierEvents(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "memory"
		if durable {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			s := NewStore()
			if durable {
				dsn := os.Getenv("OSAI_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("PostgreSQL required")
				}
				db, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				s, err = NewPostgresStore(db)
				if err != nil {
					t.Fatal(err)
				}
			}
			id := uuid.NewString()
			trade, err := s.CreateTrade("sbx:retry:"+id, "sbx:quote:"+id, "key", "sbx:corr", 95, "NGN", "USD")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetSettlementID(trade.ID, "si_failed_"+id); err != nil {
				t.Fatal(err)
			}
			if err = s.SetStatus(trade.ID, StateSettling); err != nil {
				t.Fatal(err)
			}
			failed := FinalityEvent{EventID: "sbx:failed:" + id, InstitutionID: trade.InstitutionID, SettlementID: "si_failed_" + id, TradeID: trade.ID, Status: StateFailed, CorrelationID: trade.CorrelationID}
			if err = s.ApplySettlementFinality(failed); err != nil {
				t.Fatal(err)
			}
			retry := failed
			retry.EventID = "sbx:retry:" + id
			retry.Status = StateSettling
			retry.Supersedes = failed.SettlementID
			retry.SettlementID = "si_next_" + id
			bad := retry
			bad.Supersedes = "si_unrelated"
			if s.ApplySettlementFinality(bad) == nil {
				t.Fatal("unrelated payout retry admitted")
			}
			if err = s.ApplySettlementFinality(retry); err != nil {
				t.Fatal(err)
			}
			if err = s.ApplySettlementFinality(failed); err != nil {
				t.Fatal("earlier exact failed event replay rejected", err)
			}
			confirmed := retry
			confirmed.EventID = "sbx:confirmed:" + id
			confirmed.Status = StateSettled
			confirmed.Supersedes = ""
			if err = s.ApplySettlementFinality(confirmed); err != nil {
				t.Fatal(err)
			}
			if err = s.ApplySettlementFinality(retry); err != nil {
				t.Fatal("retry replay after confirmation rejected", err)
			}
			current, _ := s.GetTrade(trade.ID)
			if current.State != StateSettled || current.SettlementID != retry.SettlementID {
				t.Fatal("earlier attempt replay regressed new payout")
			}
		})
	}
}
