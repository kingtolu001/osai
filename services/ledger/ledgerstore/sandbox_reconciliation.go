package ledgerstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxrecon"
	"strings"
)

// ReconciliationSnapshot exports actual posted fixture custody entries through
// their Ledger owner. It does not infer journal amounts from a provider status.
func (s *SandboxAccounting) ReconciliationSnapshot(ctx context.Context, institution, trade string) (sandboxrecon.Snapshot, error) {
	result := sandboxrecon.Snapshot{InstitutionID: institution, TradeID: trade, ClosingMinor: map[string]int64{"NGN": 0, "USD": 0}}
	if !strings.HasPrefix(institution, "sbx:") || trade == "" {
		return result, errors.New("sandbox reconciliation scope required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.command_json,j.kind,l.currency,sum(l.debit-l.credit)::bigint FROM ledger_sandbox_events e JOIN ledger_journals j ON j.dedup_key IN ('sbx:'||e.event_id||':0','sbx:'||e.event_id||':1') JOIN ledger_entries l ON l.journal_id=j.journal_id WHERE j.institution_id=$1 AND j.trade_id=$2 AND j.kind IN ('sandbox.fx_complete','sandbox.payout_complete','sandbox.return','sandbox.reversal') AND l.account_code IN ('sbx:NGN:custody','sbx:USD:custody') GROUP BY e.command_json,j.kind,l.currency ORDER BY j.kind,l.currency,e.command_json LIMIT 1001`, institution, trade)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var raw, kind string
		var m sandboxrecon.Movement
		if err = rows.Scan(&raw, &kind, &m.Currency, &m.DeltaMinor); err != nil {
			break
		}
		var command sandboxaccounting.Command
		if err = json.Unmarshal([]byte(raw), &command); err != nil {
			break
		}
		m.Kind = strings.TrimPrefix(kind, "sandbox.")
		if m.Kind == "fx_complete" && command.Execution != nil {
			m.Reference = command.Execution.ExecutionReference
		} else if command.Payout != nil {
			m.Reference = command.Payout.ClientReference
		}
		if m.Reference == "" {
			err = errors.New("posted fixture movement lacks immutable reference")
			break
		}
		result.Movements = append(result.Movements, m)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(result.Movements) > 1000 {
		return result, errors.New("sandbox reconciliation snapshot bound exceeded")
	}
	rows, err = tx.QueryContext(ctx, `SELECT l.currency,sum(l.debit-l.credit)::bigint FROM ledger_journals j JOIN ledger_entries l ON l.journal_id=j.journal_id WHERE j.institution_id=$1 AND l.account_code IN ('sbx:NGN:custody','sbx:USD:custody') GROUP BY l.currency`, institution)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var currency string
		var value int64
		if err = rows.Scan(&currency, &value); err != nil {
			break
		}
		result.ClosingMinor[currency] = value
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
