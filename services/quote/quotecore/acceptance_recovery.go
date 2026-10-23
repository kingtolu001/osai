package quotecore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ClaimPendingAcceptance schedules one durable, unchanged selection. Claiming
// one at a time prevents slow/invalid jobs from starving an unattempted batch.
// An ambiguous write never authorizes a new key or beneficiary.
func (s *QuoteStore) ClaimPendingAcceptance(ctx context.Context) (*ExecutableQuote, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("durable quote recovery store required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT id,body FROM quotes
 WHERE body->>'Status'='QUOTED' AND body->>'AcceptanceStartedAt' IS NOT NULL
 AND (body->>'AcceptanceStartedAt')::timestamptz <= clock_timestamp()-interval '5 seconds'
 AND (acceptance_attempted_at IS NULL OR acceptance_attempted_at <= clock_timestamp()-interval '5 seconds')
 ORDER BY COALESCE(acceptance_attempted_at,(body->>'AcceptanceStartedAt')::timestamptz),id
 LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE quotes SET acceptance_attempted_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	var q ExecutableQuote
	if err = json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	return &q, nil
}
