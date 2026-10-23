package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPostgresRateLimitSharedBurstRestartAndAtomicScopes(t *testing.T) {
	db := newIdempotencyTestDB(t)
	first, err := newPersistentRateLimiterStore(db)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("OSAI_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	otherDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	configureCustomerAPIPool(otherDB)
	second, err := newPersistentRateLimiterStore(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	// A future timestamp also verifies that backwards clock movement cannot
	// refill a bucket. Keeping refill paused makes concurrent capacity exact.
	if _, err = db.Exec(`INSERT INTO api_rate_limit_buckets(scope,tokens,last_refill_at) VALUES ('institution:shared',10,clock_timestamp()+interval '1 hour'),('credential:shared',10,clock_timestamp()+interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			limiter := first
			if i%2 == 0 {
				limiter = second
			}
			allowed, callErr := limiter.allowContext(context.Background(), "institution:shared", "credential:shared")
			if callErr != nil {
				t.Error(callErr)
				return
			}
			if allowed {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("replicas admitted %d requests from one shared burst", admitted.Load())
	}
	restarted, err := newPersistentRateLimiterStore(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "institution:shared", "credential:new"); err != nil || allowed {
		t.Fatal("restart reset institution capacity", err)
	}
	var tokens float64
	if err = db.QueryRow(`SELECT tokens FROM api_rate_limit_buckets WHERE scope='credential:new'`).Scan(&tokens); err != nil || tokens != 10 {
		t.Fatal("denied institution consumed unrelated credential capacity", tokens, err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "institution:new", "credential:new"); err != nil || !allowed {
		t.Fatal("unrelated institution was throttled", err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "credential:shared", "institution:another"); err != nil || allowed {
		t.Fatal("credential scope escaped its shared limit", err)
	}
	if _, err = db.Exec(`UPDATE api_rate_limit_buckets SET last_refill_at=clock_timestamp()-interval '1 minute' WHERE scope IN ('institution:shared','credential:shared')`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "institution:shared", "credential:shared"); err != nil || !allowed {
		t.Fatal("elapsed time did not refill capacity", err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "duplicate", "duplicate"); err == nil || allowed {
		t.Fatal("duplicate scopes accepted")
	}
	if err = otherDB.Close(); err != nil {
		t.Fatal(err)
	}
	if allowed, err := restarted.allowContext(context.Background(), "institution:new", "credential:new"); err == nil || allowed {
		t.Fatal("database failure bypassed shared rate limit")
	}
}
