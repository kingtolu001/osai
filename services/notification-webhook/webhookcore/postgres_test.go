package webhookcore

import (
	"context"
	"database/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresRetryAndDeadLetter(t *testing.T) {
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
	store := &Postgres{DB: db, Client: receiver.Client(), Secret: "local-test-secret", RetryDelay: time.Millisecond, MaxAttempts: 2}
	if err = store.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	if err = store.Configure(inst, receiver.URL, "env:OSAI_WEBHOOK_SECRET", []string{"quote.accepted"}); err != nil {
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
		if err != nil || !done {
			t.Fatalf("attempt %d: done=%v err=%v", n+1, done, err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	status, count, err := store.EventStatus(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "DEAD_LETTER" || count != 2 {
		t.Fatalf("status=%s attempts=%d", status, count)
	}
	var logical, attempts int
	if err = db.QueryRow(`SELECT count(*) FROM outbound_events WHERE event_id=$1`, eventID).Scan(&logical); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM webhook_delivery_attempts WHERE event_id=$1`, eventID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if logical != 1 || attempts != 2 {
		t.Fatalf("logical=%d attempts=%d", logical, attempts)
	}
}
