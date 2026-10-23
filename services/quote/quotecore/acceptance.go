package quotecore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrQuoteAcceptanceSelection = errors.New("quote acceptance selection conflict")
var ErrQuoteNotExecutable = errors.New("quote is not executable")

// BeginAcceptance freezes the caller's selection before the first Trade RPC.
// An ambiguous response can then be resumed after quote expiry using the same
// immutable selection. Trade still refuses to create a new expired trade.
func (s *QuoteStore) BeginAcceptance(id, key, beneficiary, correlation string, now time.Time) (*ExecutableQuote, error) {
	return s.BeginAcceptanceContext(context.Background(), id, key, beneficiary, correlation, now)
}

func (s *QuoteStore) BeginAcceptanceContext(ctx context.Context, id, key, beneficiary, correlation string, now time.Time) (*ExecutableQuote, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrQuoteAcceptanceSelection
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		q, ok := s.quotes[id]
		if !ok {
			return nil, ErrQuoteNotExecutable
		}
		if err := beginAcceptance(q, key, beneficiary, correlation, now); err != nil {
			return nil, err
		}
		return cloneExecutableQuote(q), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT body FROM quotes WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var q ExecutableQuote
	if err = json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	if err = beginAcceptance(&q, key, beneficiary, correlation, now); err != nil {
		return nil, err
	}
	raw, err = json.Marshal(&q)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE quotes SET body=$2 WHERE id=$1`, id, string(raw)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &q, nil
}

func beginAcceptance(q *ExecutableQuote, key, beneficiary, correlation string, now time.Time) error {
	if q.Status != QuoteStatusQuoted {
		return ErrQuoteNotExecutable
	}
	if q.AcceptanceStartedAt != nil {
		if q.AcceptanceKey != key || q.AcceptanceBeneficiary != beneficiary {
			return ErrQuoteAcceptanceSelection
		}
		return nil
	}
	if q.Status != QuoteStatusQuoted || !now.Before(q.ExpiresAt) {
		return ErrQuoteNotExecutable
	}
	started := now.UTC()
	q.AcceptanceStartedAt = &started
	q.AcceptanceKey, q.AcceptanceBeneficiary, q.AcceptanceCorrelation = key, beneficiary, correlation
	return nil
}

func validateAcceptanceCompletion(q *ExecutableQuote, key string, now time.Time) error {
	if q.AcceptanceStartedAt != nil {
		if q.AcceptanceKey != key || !q.AcceptanceStartedAt.Before(q.ExpiresAt) {
			return ErrQuoteAcceptanceSelection
		}
		return nil
	}
	if !now.Before(q.ExpiresAt) {
		return ErrQuoteNotExecutable
	}
	return nil
}
