package settlementstore

import (
	"context"
	"errors"
	"time"
)

type SandboxPayoutMetrics struct {
	Unresolved, Unknown     int64
	OldestUnresolvedSeconds float64
}

func (p Postgres) SandboxPayoutMetrics(ctx context.Context) (SandboxPayoutMetrics, error) {
	var m SandboxPayoutMetrics
	err := p.DB.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE state='UNKNOWN'),COALESCE(max(EXTRACT(EPOCH FROM (now()-created_at))),0) FROM settlement_instructions WHERE provider_id='sbx:provider' AND state NOT IN ('CONFIRMED','FAILED','RETURNED','REVERSED','MANUAL_REVIEW')`).Scan(&m.Unresolved, &m.Unknown, &m.OldestUnresolvedSeconds)
	return m, err
}

// Escalation is an operational fact only. It neither fails a payout, releases
// funds nor authorizes another submission. The existing durable notification
// relay delivers it only to an explicitly configured subscriber.
func (p Postgres) EscalateSandboxUnknownPayouts(ctx context.Context, age time.Duration, limit int) error {
	if age < time.Second || age > 24*time.Hour || limit < 1 || limit > 100 {
		return errors.New("bounded sandbox UNKNOWN alert threshold required")
	}
	_, err := p.DB.ExecContext(ctx, `INSERT INTO settlement_domain_outbox(event_id,institution_id,settlement_id,trade_id,event_type,correlation_id,occurred_at)
 SELECT 'sandbox_unknown_alert:'||id,institution_id,id,trade_id,'si.unknown_escalated',correlation_id,now()
 FROM settlement_instructions WHERE provider_id='sbx:provider' AND state='UNKNOWN' AND created_at<=now()-($1*interval '1 second') AND trade_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM settlement_domain_outbox a WHERE a.event_id='sandbox_unknown_alert:'||settlement_instructions.id)
 ORDER BY created_at,id LIMIT $2 ON CONFLICT(event_id) DO NOTHING`, age.Seconds(), limit)
	return err
}
