package ledgerstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/osai/osai/pkg/sandboxaccounting"
)

// Position is an authenticated service snapshot, never authority to dispatch.
// Only a committed payout_bind followed by payout_submitted grants an attempt.
func (s *SandboxAccounting) Position(ctx context.Context, institution, trade string) (sandboxaccounting.Position, error) {
	var result sandboxaccounting.Position
	if !strings.HasPrefix(institution, "sbx:") || trade == "" {
		return result, errors.New("sandbox position identity required")
	}
	var command, binding, bindingState string
	err := s.db.QueryRowContext(ctx, `SELECT p.state,p.revision,e.command_json,COALESCE(b.binding_json,''),COALESCE(b.state,'')
 FROM ledger_sandbox_positions p
 JOIN LATERAL (SELECT command_json FROM ledger_sandbox_events WHERE trade_id=p.trade_id ORDER BY created_at,event_id LIMIT 1) e ON true
 LEFT JOIN LATERAL (SELECT binding_json,state FROM ledger_sandbox_payout_bindings WHERE trade_id=p.trade_id ORDER BY bound_revision DESC LIMIT 1) b ON true
 WHERE p.trade_id=$1 AND e.command_json::jsonb->'Terms'->>'InstitutionID'=$2`, trade, institution).Scan(&result.State, &result.Revision, &command, &binding, &bindingState)
	if err != nil {
		return result, err
	}
	var c SandboxAccountingCommand
	if err = json.Unmarshal([]byte(command), &c); err != nil {
		return result, err
	}
	result.Terms = c.Terms
	if binding != "" {
		result.Payout = &sandboxaccounting.PayoutBinding{}
		if err = json.Unmarshal([]byte(binding), result.Payout); err != nil {
			return result, err
		}
		result.PayoutState = bindingState
	}
	return result, nil
}

func applySandboxPayoutBinding(ctx context.Context, tx *sql.Tx, c SandboxAccountingCommand, state string, revision int64) error {
	var raw, previousState string
	err := tx.QueryRowContext(ctx, `SELECT binding_json,state FROM ledger_sandbox_payout_bindings WHERE trade_id=$1 ORDER BY bound_revision DESC LIMIT 1 FOR UPDATE`, c.Terms.TradeID).Scan(&raw, &previousState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hasBinding := err == nil
	if c.Payout == nil {
		if c.Event == "payout_bind" || c.Event == "payout_submitted" || (hasBinding && (c.Event == "unknown" || c.Event == "payout_complete" || c.Event == "payout_failed" || c.Event == "return" || c.Event == "reversal")) {
			return errors.New("bound sandbox payout identity required")
		}
		// Historical manifest fixtures may exercise accounting directly. An
		// accepted runtime trade must always bind its destination instruction.
		if strings.HasPrefix(c.Terms.TradeID, "trd_") && c.Terms.Plan != nil && (c.Event == "payout_complete" || c.Event == "payout_failed" || c.Event == "return" || c.Event == "reversal") {
			return errors.New("runtime FX payout requires a bound instruction")
		}
		return nil
	}
	b := c.Payout
	if c.Terms.Plan == nil || b.SettlementID == "" || b.ClientReference == "" || b.ProviderID != "sbx:provider" || !sandboxaccounting.ValidBeneficiaryID(b.BeneficiaryID) || b.Currency != "USD" || b.AmountMinor != c.Terms.Plan.DestinationMinor {
		return errors.New("payout binding differs from accepted sandbox destination")
	}
	if (c.Terms.Plan.BeneficiaryID != "" && b.BeneficiaryID != c.Terms.Plan.BeneficiaryID) || (c.Terms.Plan.ProviderID != "" && b.ProviderID != c.Terms.Plan.ProviderID) {
		return errors.New("payout substituted the accepted beneficiary or provider")
	}
	if c.Event == "payout_bind" {
		if state != "FX_HELD" || (hasBinding && previousState != "FAILED") {
			return errors.New("sandbox FX proceeds already bound or unavailable")
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ledger_sandbox_payout_bindings(settlement_id,client_reference,trade_id,binding_json,state,bound_revision) VALUES($1,$2,$3,$4,'BOUND',$5)`, b.SettlementID, b.ClientReference, c.Terms.TradeID, string(encoded), revision+1)
		return err
	}
	var previous sandboxaccounting.PayoutBinding
	if !hasBinding || json.Unmarshal([]byte(raw), &previous) != nil || !reflect.DeepEqual(previous, *b) {
		return errors.New("sandbox payout identity conflict")
	}
	next := ""
	switch c.Event {
	case "payout_submitted":
		if previousState == "BOUND" {
			next = "SUBMITTED"
		}
	case "unknown":
		if previousState == "SUBMITTED" {
			next = "UNKNOWN"
		}
	case "payout_complete":
		if previousState == "SUBMITTED" || previousState == "UNKNOWN" {
			next = "COMPLETE"
		}
	case "payout_failed":
		if previousState == "SUBMITTED" || previousState == "UNKNOWN" {
			next = "FAILED"
		}
	case "return", "reversal":
		if previousState == "COMPLETE" {
			next = "RETURNED"
		}
	default:
		return errors.New("payout identity not applicable to this sandbox event")
	}
	if next == "" {
		return errors.New("sandbox payout attempt transition prohibited")
	}
	_, err = tx.ExecContext(ctx, `UPDATE ledger_sandbox_payout_bindings SET state=$2 WHERE settlement_id=$1`, b.SettlementID, next)
	return err
}
