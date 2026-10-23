package tradecore

import "testing"

func TestReturnedAndReversedFinalityRequireVerifiedPriorSettlement(t *testing.T) {
	for _, next := range []TradeState{StateReturned, StateReversed} {
		t.Run(string(next), func(t *testing.T) {
			s := NewStore()
			trade, err := s.CreateTrade("sbx:institution", "sbx:quote", "sbx:key", "sbx:correlation", 9500, "NGN", "USD")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetSettlementID(trade.ID, "si_fixture"); err != nil {
				t.Fatal(err)
			}
			if err = s.SetStatus(trade.ID, StateSettling); err != nil {
				t.Fatal(err)
			}
			returned := FinalityEvent{EventID: "sbx:return", InstitutionID: trade.InstitutionID, SettlementID: "si_fixture", TradeID: trade.ID, Status: next, CorrelationID: trade.CorrelationID}
			if err = s.ApplySettlementFinality(returned); err == nil {
				t.Fatal("unconfirmed payout returned")
			}
			confirmed := returned
			confirmed.EventID = "sbx:confirmed"
			confirmed.Status = StateSettled
			if err = s.ApplySettlementFinality(confirmed); err != nil {
				t.Fatal(err)
			}
			if err = s.ApplySettlementFinality(returned); err != nil {
				t.Fatal(err)
			}
			if err = s.ApplySettlementFinality(returned); err != nil {
				t.Fatal(err)
			}
			if err = s.ApplySettlementFinality(confirmed); err != nil {
				t.Fatal("exact earlier confirmation replay lost idempotency", err)
			}
			current, _ := s.GetTrade(trade.ID)
			if current.State != next {
				t.Fatal("old confirmation regressed a returned payout")
			}
		})
	}
}
