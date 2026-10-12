package webhookcore

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresRetryAndDeadLetter(t *testing.T) {
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "true")
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminDB := db
	schema := "phase5_webhook_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); adminDB.Close() })
	isolated, err := sql.Open("pgx", dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Close()
	db = isolated
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer receiver.Close()
	id := uuid.NewString()
	inst := "inst_test_" + id
	eventID := "evt_test_" + id
	store := &Postgres{DB: db, Client: receiver.Client(), ConfigEncryptionKey: []byte("local-test-config-encryption-key-with-more-than-32-bytes"), RetryDelay: time.Millisecond, MaxAttempts: 2}
	if err = store.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	if err = store.Configure(inst, receiver.URL, "local-test-secret", []string{"quote.accepted"}); err != nil {
		t.Fatal(err)
	}
	event := &notificationv1.Event{EventId: eventID, InstitutionId: inst, EventType: "quote.accepted", CorrelationId: "corr_test_" + id, AggregateId: "quo_test_" + id, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), PayloadJson: `{"status":"ACCEPTED"}`}
	if err = store.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err = store.Publish(context.Background(), event); err != nil {
		t.Fatal("same event replay", err)
	}
	for n := 0; n < 2; n++ {
		done, err := store.DeliverDue(context.Background())
		if err == nil || !done {
			t.Fatalf("attempt %d: done=%v err=%v", n+1, done, err)
		}
		if _, err = db.Exec(`UPDATE outbound_events SET next_attempt_at=now() WHERE event_id=$1`, eventID); err != nil {
			t.Fatal(err)
		}
	}
	status, count, err := store.EventStatus(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "DEAD_LETTER" || count != 2 {
		t.Fatalf("status=%s attempts=%d", status, count)
	}
	replayKey := "replay-" + id
	const concurrentReplayRequests = 8
	var replayWG sync.WaitGroup
	replayErrors := make(chan error, concurrentReplayRequests)
	for n := 0; n < concurrentReplayRequests; n++ {
		replayWG.Add(1)
		go func() {
			defer replayWG.Done()
			replayErrors <- store.Replay(context.Background(), inst, eventID, replayKey)
		}()
	}
	replayWG.Wait()
	close(replayErrors)
	for err := range replayErrors {
		if err != nil {
			t.Fatalf("concurrent same-key replay: %v", err)
		}
	}
	if err := store.Replay(context.Background(), inst, "different-event", replayKey); !errors.Is(err, ErrReplayKeyConflict) {
		t.Fatalf("reusing replay key for another event: got %v, want conflict", err)
	}
	if err := store.Replay(context.Background(), "another-institution", eventID, "foreign-key"); !errors.Is(err, ErrReplayEventNotFound) {
		t.Fatalf("cross-institution replay: got %v, want not found", err)
	}
	restartedStore := &Postgres{DB: db, Client: receiver.Client(), ConfigEncryptionKey: []byte("local-test-config-encryption-key-with-more-than-32-bytes"), RetryDelay: time.Millisecond, MaxAttempts: 2}
	if err := restartedStore.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	if err := restartedStore.Replay(context.Background(), inst, eventID, replayKey); err != nil {
		t.Fatalf("replay idempotency must survive store restart: %v", err)
	}
	for n := 0; n < 2; n++ {
		done, err := store.DeliverDue(context.Background())
		if err == nil || !done {
			t.Fatalf("replay attempt %d: done=%v err=%v", n+1, done, err)
		}
		if _, err := db.Exec(`UPDATE outbound_events SET next_attempt_at=now() WHERE event_id=$1`, eventID); err != nil {
			t.Fatal(err)
		}
	}
	status, count, err = store.EventStatus(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "DEAD_LETTER" || count != 4 {
		t.Fatalf("replayed event status=%s attempts=%d, want DEAD_LETTER and 4 total attempts", status, count)
	}
	var logical, attempts int
	if err = db.QueryRow(`SELECT count(*) FROM outbound_events WHERE event_id=$1`, eventID).Scan(&logical); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM webhook_delivery_attempts WHERE event_id=$1`, eventID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if logical != 1 || attempts != 4 {
		t.Fatalf("logical=%d attempts=%d", logical, attempts)
	}
	var replayRequests int
	if err = db.QueryRow(`SELECT count(*) FROM webhook_delivery_replays WHERE event_id=$1`, eventID).Scan(&replayRequests); err != nil {
		t.Fatal(err)
	}
	if replayRequests != 1 {
		t.Fatalf("expected one durable replay request, got %d", replayRequests)
	}
	var encrypted []byte
	if err = db.QueryRow(`SELECT secret_ciphertext FROM webhook_config WHERE institution_id=$1`, inst).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), "local-test-secret") {
		t.Fatal("webhook signing secret was stored in plaintext")
	}

	t.Setenv("OSAI_LOCAL_PROOF_MODE", "false")
	rejectedInstitution := "inst_rejected_" + id
	rejectedEventID := "evt_rejected_" + id
	rejectedSecret, err := store.encryptSecret("local-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO webhook_config(institution_id,endpoint_url,secret_ref,secret_ciphertext,status,subscriptions) VALUES($1,$2,'encrypted',$3,'ACTIVE',$4)`, rejectedInstitution, "https://127.0.0.1/webhook", rejectedSecret, []string{"quote.accepted"}); err != nil {
		t.Fatal(err)
	}
	rejectedEvent := &notificationv1.Event{EventId: rejectedEventID, InstitutionId: rejectedInstitution, EventType: "quote.accepted", CorrelationId: "corr_rejected_" + id, AggregateId: "quo_rejected_" + id, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), PayloadJson: `{"status":"ACCEPTED"}`}
	if err = store.Publish(context.Background(), rejectedEvent); err != nil {
		t.Fatal(err)
	}
	done, err := store.DeliverDue(context.Background())
	if err == nil || !done {
		t.Fatalf("rejected destination attempt: done=%v err=%v", done, err)
	}
	status, count, err = store.EventStatus(rejectedEventID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "DEAD_LETTER" || count != 1 {
		t.Fatalf("rejected destination status=%s attempts=%d, want DEAD_LETTER and 1", status, count)
	}
	if err = db.QueryRow(`SELECT count(*) FROM webhook_delivery_attempts WHERE event_id=$1`, rejectedEventID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("rejected destination has %d delivery attempts, want 1", attempts)
	}
}
