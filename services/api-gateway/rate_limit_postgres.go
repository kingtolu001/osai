package main

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// Runtime gateways share their existing owner database. They never fall back
// to process-local buckets after a database error or a gateway restart.
func newPersistentRateLimiterStore(db *sql.DB) (*rateLimiterStore, error) {
	if db == nil {
		return nil, errors.New("rate limit database required")
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_rate_limit_buckets (
 scope text PRIMARY KEY, tokens double precision NOT NULL CHECK (tokens >= 0 AND tokens <= 10), last_refill_at timestamptz NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return &rateLimiterStore{db: db}, nil
}

func (r *rateLimiterStore) allowContext(ctx context.Context, keys ...string) (bool, error) {
	if r == nil {
		return false, errors.New("rate limiter unavailable")
	}
	if r.db == nil {
		return r.allow(keys...), nil
	}
	if len(keys) == 0 {
		return false, errors.New("rate limit scopes required")
	}
	ordered := append([]string(nil), keys...)
	sort.Strings(ordered)
	for i, key := range ordered {
		if key == "" || strings.HasSuffix(key, ":") || (i > 0 && ordered[i-1] == key) {
			return false, errors.New("invalid rate limit scopes")
		}
	}
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(call, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	type bucket struct {
		key    string
		tokens float64
		last   time.Time
	}
	buckets := make([]bucket, 0, len(ordered))
	// All replicas lock every participating scope in canonical order. No scope
	// is charged if any other scope denies the request.
	for _, key := range ordered {
		if _, err = tx.ExecContext(call, `INSERT INTO api_rate_limit_buckets(scope,tokens,last_refill_at) VALUES($1,$2,clock_timestamp()) ON CONFLICT DO NOTHING`, key, rateLimitBurst); err != nil {
			return false, err
		}
		b := bucket{key: key}
		if err = tx.QueryRowContext(call, `SELECT tokens,last_refill_at FROM api_rate_limit_buckets WHERE scope=$1 FOR UPDATE`, key).Scan(&b.tokens, &b.last); err != nil {
			return false, err
		}
		buckets = append(buckets, b)
	}
	var now time.Time
	if err = tx.QueryRowContext(call, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, err
	}
	allowed := true
	for i := range buckets {
		b := &buckets[i]
		if now.After(b.last) {
			b.tokens = math.Min(rateLimitBurst, b.tokens+now.Sub(b.last).Seconds()*rateLimitPerMinute/60)
			b.last = now
		}
		if b.tokens < 1 {
			allowed = false
		}
	}
	for _, b := range buckets {
		if allowed {
			b.tokens--
		}
		if _, err = tx.ExecContext(call, `UPDATE api_rate_limit_buckets SET tokens=$2,last_refill_at=$3 WHERE scope=$1`, b.key, b.tokens, b.last); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return allowed, nil
}
