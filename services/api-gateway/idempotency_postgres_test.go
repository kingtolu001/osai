package main

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func newIdempotencyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("PostgreSQL DSN not configured")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open PostgreSQL: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	schema := "phase5_idempotency_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	isolated, err := sql.Open("pgx", dsn+"&search_path="+schema)
	if err != nil {
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	if err := ensureCustomerAPISchema(isolated); err != nil {
		isolated.Close()
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		isolated.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return isolated
}

func TestPostgresIdempotencyCrashRecoveryAndOriginalResponse(t *testing.T) {
	db := newIdempotencyTestDB(t)
	store := &idempotencyStore{db: db, rows: map[string]*idempotencyEntry{}}
	payload := map[string]any{"amount": 100, "currency": "NGN"}
	replayed, code, _, firstToken, err := store.CheckAndStore("inst_a", "/mutate", "key-1", payload, 202, nil)
	if err != nil || replayed || code != 202 || firstToken == "" {
		t.Fatalf("initial reservation: replay=%v code=%d token=%q err=%v", replayed, code, firstToken, err)
	}
	if _, _, _, _, err := store.CheckAndStore("inst_a", "/mutate", "key-1", map[string]any{"currency": "NGN", "amount": 100}, 202, nil); !errors.Is(err, errIdempotencyInProgress) {
		t.Fatalf("canonical equivalent concurrent request should be in progress, got %v", err)
	}
	if _, _, _, _, err := store.CheckAndStore("inst_a", "/mutate", "key-1", map[string]any{"amount": 101, "currency": "NGN"}, 202, nil); !errors.Is(err, errIdempotencyConflict) {
		t.Fatalf("changed payload should conflict, got %v", err)
	}
	if _, err := db.Exec(`UPDATE customer_api_idempotency SET locked_until=now()-interval '1 second' WHERE institution_id='inst_a' AND endpoint='/mutate' AND idempotency_key='key-1'`); err != nil {
		t.Fatal(err)
	}
	_, _, _, recoveredToken, err := store.CheckAndStore("inst_a", "/mutate", "key-1", payload, 202, nil)
	if err != nil || recoveredToken == "" || recoveredToken == firstToken {
		t.Fatalf("expired process reservation was not safely reclaimed: token=%q err=%v", recoveredToken, err)
	}
	if err := store.SaveResponse("inst_a", "/mutate", "key-1", payload, firstToken, 202, map[string]any{"operation_id": "op_original"}); !errors.Is(err, errIdempotencyConflict) {
		t.Fatalf("stale process token should not complete recovered reservation, got %v", err)
	}
	original := map[string]any{"operation_id": "op_original", "request_id": "req_original"}
	if err := store.SaveResponse("inst_a", "/mutate", "key-1", payload, recoveredToken, 202, original); err != nil {
		t.Fatal(err)
	}
	restarted := &idempotencyStore{db: db, rows: map[string]*idempotencyEntry{}}
	replayed, code, response, token, err := restarted.CheckAndStore("inst_a", "/mutate", "key-1", map[string]any{"currency": "NGN", "amount": 100}, 202, nil)
	if err != nil || !replayed || code != 202 || token != "" || response["operation_id"] != "op_original" || response["request_id"] != "req_original" {
		t.Fatalf("restart replay did not return stored original response: replay=%v code=%d token=%q body=%v err=%v", replayed, code, token, response, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM customer_api_idempotency WHERE institution_id='inst_a' AND endpoint='/mutate' AND idempotency_key='key-1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("expected one durable idempotency row, got %d", rows)
	}
}

func TestPostgresConcurrentIdempotencyReservationHasSingleOwner(t *testing.T) {
	db := newIdempotencyTestDB(t)
	store := &idempotencyStore{db: db, rows: map[string]*idempotencyEntry{}}
	const callers = 8
	start := make(chan struct{})
	type outcome struct {
		token string
		err   error
	}
	results := make(chan outcome, callers)
	var wait sync.WaitGroup
	for n := 0; n < callers; n++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _, _, token, err := store.CheckAndStore("inst_a", "/mutate", "concurrent-key", map[string]any{"amount": 100}, 200, nil)
			results <- outcome{token: token, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	owners, inProgress := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.token != "":
			owners++
		case errors.Is(result.err, errIdempotencyInProgress):
			inProgress++
		default:
			t.Fatalf("unexpected reservation result: %+v", result)
		}
	}
	if owners != 1 || inProgress != callers-1 {
		t.Fatalf("expected one reservation owner and %d in-progress replies, got owners=%d in_progress=%d", callers-1, owners, inProgress)
	}
}

func TestIdempotencyDatabaseOutageDoesNotReserveOrSucceed(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://localhost/unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := &idempotencyStore{db: db, rows: map[string]*idempotencyEntry{}}
	if _, _, _, token, err := store.CheckAndStore("inst_a", "/mutate", "key-1", map[string]any{"amount": 100}, 200, nil); err == nil || token != "" {
		t.Fatalf("database outage must fail without an acquired reservation, token=%q err=%v", token, err)
	}
}
