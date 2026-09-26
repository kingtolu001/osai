package webhookcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"net/http"
	"time"
)

// Postgres owns logical customer events and delivery history. Signing secrets
// stay in process configuration; only an opaque secret reference is stored.
type Postgres struct {
	DB          *sql.DB
	Client      *http.Client
	Secret      string
	RetryDelay  time.Duration
	MaxAttempts int
}

func (p *Postgres) EnsureSchema() error {
	_, err := p.DB.Exec(`CREATE TABLE IF NOT EXISTS webhook_config (institution_id text PRIMARY KEY, endpoint_url text NOT NULL, secret_ref text NOT NULL, status text NOT NULL, subscriptions text[] NOT NULL);
 CREATE TABLE IF NOT EXISTS outbound_events (event_id text PRIMARY KEY, institution_id text NOT NULL, event_type text NOT NULL, correlation_id text NOT NULL, aggregate_id text NOT NULL, body jsonb NOT NULL, status text NOT NULL DEFAULT 'PENDING', attempt_count int NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(), occurred_at timestamptz NOT NULL);
 CREATE TABLE IF NOT EXISTS webhook_delivery_attempts (event_id text NOT NULL REFERENCES outbound_events(event_id), attempt_number int NOT NULL, attempted_at timestamptz NOT NULL DEFAULT now(), http_status int NOT NULL DEFAULT 0, outcome text NOT NULL, completed_at timestamptz, PRIMARY KEY(event_id,attempt_number));`)
	return err
}
func (p *Postgres) Configure(institution, url, secretRef string, subscriptions []string) error {
	if institution == "" || url == "" || secretRef == "" || len(subscriptions) == 0 {
		return errors.New("webhook configuration incomplete")
	}
	_, err := p.DB.Exec(`INSERT INTO webhook_config(institution_id,endpoint_url,secret_ref,status,subscriptions) VALUES($1,$2,$3,'ACTIVE',$4) ON CONFLICT(institution_id) DO UPDATE SET endpoint_url=excluded.endpoint_url,secret_ref=excluded.secret_ref,status=excluded.status,subscriptions=excluded.subscriptions`, institution, url, secretRef, subscriptions)
	return err
}
func (p *Postgres) Publish(ctx context.Context, event *notificationv1.Event) error {
	if event == nil || event.EventId == "" || event.InstitutionId == "" || event.EventType == "" || event.CorrelationId == "" || event.AggregateId == "" || !json.Valid([]byte(event.PayloadJson)) {
		return errors.New("incomplete event")
	}
	occurred, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
	if err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	var existing []byte
	err = p.DB.QueryRowContext(ctx, `INSERT INTO outbound_events(event_id,institution_id,event_type,correlation_id,aggregate_id,body,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO UPDATE SET event_id=excluded.event_id RETURNING body`, event.EventId, event.InstitutionId, event.EventType, event.CorrelationId, event.AggregateId, string(body), occurred).Scan(&existing)
	if err != nil {
		return err
	}
	var prior notificationv1.Event
	if err = json.Unmarshal(existing, &prior); err != nil {
		return err
	}
	if prior.EventId != event.EventId || prior.InstitutionId != event.InstitutionId || prior.PayloadJson != event.PayloadJson {
		return errors.New("event id payload conflict")
	}
	return nil
}

// DeliverDue processes one due event. The row lock keeps a single attempt
// number across workers. A crash rolls the attempt back and leaves it due.
func (p *Postgres) DeliverDue(ctx context.Context) (bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id, inst, eventType, endpoint, secretRef string
	var body []byte
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT e.event_id,e.institution_id,e.event_type,e.body,e.attempt_count,c.endpoint_url,c.secret_ref FROM outbound_events e JOIN webhook_config c ON c.institution_id=e.institution_id AND c.status='ACTIVE' AND e.event_type=ANY(c.subscriptions) WHERE e.status IN ('PENDING','RETRY') AND e.next_attempt_at<=now() ORDER BY e.next_attempt_at,e.event_id LIMIT 1 FOR UPDATE OF e SKIP LOCKED`).Scan(&id, &inst, &eventType, &body, &attempts, &endpoint, &secretRef)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if secretRef != "env:OSAI_WEBHOOK_SECRET" || p.Secret == "" {
		return false, errors.New("webhook secret unavailable")
	}
	var ev notificationv1.Event
	if err = json.Unmarshal(body, &ev); err != nil {
		return false, err
	}
	attempt, deliveryErr := DeliverCustomerEvent(p.Client, CustomerWebhookConfig{InstitutionID: inst, EndpointURL: endpoint, Secret: p.Secret}, OutboundEvent{EventID: id, InstitutionID: inst, EventType: eventType, EventVersion: "v1", CorrelationID: ev.CorrelationId, Payload: ev.PayloadJson, OccurredAt: parseTime(ev.OccurredAt)})
	if attempt == nil {
		return false, deliveryErr
	}
	number := attempts + 1
	max := p.MaxAttempts
	if max < 1 {
		max = 3
	}
	delay := p.RetryDelay
	if delay <= 0 {
		delay = time.Minute
	}
	next := time.Now().Add(delay * time.Duration(1<<min(attempts, 8)))
	statusValue := "RETRY"
	if attempt.Outcome == "success" {
		statusValue = "SUCCESS"
	} else if number >= max || attempt.Outcome == "failed" {
		statusValue = "DEAD_LETTER"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_delivery_attempts(event_id,attempt_number,http_status,outcome,completed_at) VALUES($1,$2,$3,$4,now())`, id, number, attempt.HTTPStatus, attempt.Outcome); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_events SET status=$2,attempt_count=$3,next_attempt_at=$4 WHERE event_id=$1`, id, statusValue, number, next); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	if deliveryErr != nil {
		return true, nil
	}
	return true, nil
}
func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func (p *Postgres) EventStatus(id string) (string, int, error) {
	var status string
	var count int
	err := p.DB.QueryRow(`SELECT status,attempt_count FROM outbound_events WHERE event_id=$1`, id).Scan(&status, &count)
	return status, count, err
}
func (p *Postgres) String() string { return fmt.Sprintf("notification database ready") }
