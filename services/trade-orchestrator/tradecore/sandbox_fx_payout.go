package tradecore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/osai/osai/pkg/sandboxpolicy"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
)

// RelaySandboxFXPayouts asks Settlement to create the one durable destination
// instruction only after Ledger's FX posting receipt has been committed.
func (s *Store) RelaySandboxFXPayouts(ctx context.Context, client settlementv1.SettlementServiceClient, limit int) (int, error) {
	enabled, err := sandboxpolicy.FXPayout()
	if err != nil || !enabled {
		return 0, err
	}
	if client == nil || limit < 1 || limit > 100 {
		return 0, errors.New("bounded sandbox payout relay required")
	}
	if err = s.validateSandboxPosting(ctx); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.body FROM trade_sandbox_fx_postings p JOIN trade_fx_executions e ON e.execution_id=p.execution_id JOIN trades t ON t.id=e.trade_id WHERE p.posted_at IS NOT NULL AND t.status='FX_EXECUTED' AND COALESCE(t.body->>'SettlementID','')='' ORDER BY COALESCE(p.payout_attempted_at,p.posted_at),p.event_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var trades []Trade
	for rows.Next() {
		var raw []byte
		var trade Trade
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &trade); err != nil {
			break
		}
		trades = append(trades, trade)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	processed := 0
	var failures []error
	for _, trade := range trades {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE trade_sandbox_fx_postings SET payout_attempted_at=now() WHERE execution_id=$1`, trade.FXExecutionID); err != nil {
			failures = append(failures, err)
			continue
		}
		p := trade.PricingSnapshot
		allowed, policyErr := sandboxpolicy.FXTradeAllowed(trade.InstitutionID, trade.BaseCurrency, trade.QuoteCurrency, trade.SettlementProviderID)
		if policyErr != nil || !allowed || p == nil {
			failures = append(failures, errors.New("sandbox payout differs from accepted trade"))
			continue
		}
		response, callErr := client.CreateSettlementForTrade(sandboxpolicy.Outgoing(ctx), &settlementv1.CreateSettlementForTradeRequest{InstitutionId: trade.InstitutionID, TradeId: trade.ID, QuoteId: trade.QuoteID, IdempotencyKey: trade.IdempotencyKey, CorrelationId: trade.CorrelationID, BeneficiaryId: trade.BeneficiaryID, Amount: &settlementv1.Money{AmountMinor: p.DestinationAmountMinor, Currency: p.DestinationCurrency}, Purpose: "sandbox FX destination payout", ProviderId: trade.SettlementProviderID})
		if callErr != nil {
			failures = append(failures, errors.New("sandbox destination instruction unresolved"))
			continue
		}
		if response == nil || response.SettlementId == "" || response.InstitutionId != trade.InstitutionID || response.TradeId != trade.ID || response.QuoteId != trade.QuoteID || response.ProviderId != trade.SettlementProviderID || response.Beneficiary != trade.BeneficiaryID || response.AmountMinor != p.DestinationAmountMinor || response.Currency != p.DestinationCurrency || response.CorrelationId != trade.CorrelationID {
			failures = append(failures, errors.New("sandbox destination receipt mismatch"))
			continue
		}
		if err = s.attachSandboxPayout(ctx, trade.ID, response.SettlementId); err != nil {
			failures = append(failures, err)
			continue
		}
		processed++
	}
	return processed, errors.Join(failures...)
}

func (s *Store) attachSandboxPayout(ctx context.Context, tradeID, settlementID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT body FROM trades WHERE id=$1 FOR UPDATE`, tradeID).Scan(&raw); err != nil {
		return err
	}
	var trade Trade
	if err = json.Unmarshal(raw, &trade); err != nil {
		return err
	}
	if trade.SettlementID != "" {
		if trade.SettlementID != settlementID {
			return errors.New("sandbox destination instruction conflict")
		}
		return tx.Commit()
	}
	if trade.State != StateFXExecuted {
		return errors.New("verified FX state required for payout attachment")
	}
	previous := trade.Status
	trade.SettlementID, trade.State, trade.Status, trade.UpdatedAt = settlementID, StateSettling, string(StateSettling), time.Now().UTC()
	raw, err = json.Marshal(trade)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE trades SET body=$2,status=$3 WHERE id=$1`, tradeID, string(raw), trade.Status); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO trade_transitions(trade_id,from_state,to_state) VALUES($1,$2,$3)`, tradeID, previous, trade.Status); err != nil {
		return err
	}
	return tx.Commit()
}
