package webhookcore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
)

// Postgres owns customer event delivery history and encrypted signing secrets.
type Postgres struct {
	DB                  *sql.DB
	Client              *http.Client
	ConfigEncryptionKey []byte
	RetryDelay          time.Duration
	MaxAttempts         int
}

var (
	ErrReplayEventNotFound = errors.New("webhook event not found")
	ErrReplayNotAllowed    = errors.New("webhook event is not eligible for replay")
	ErrReplayKeyConflict   = errors.New("webhook replay idempotency key conflict")
)

func (p *Postgres) EnsureSchema() error {
	_, err := p.DB.Exec(`CREATE TABLE IF NOT EXISTS webhook_config (institution_id text PRIMARY KEY, endpoint_url text NOT NULL, secret_ref text NOT NULL, secret_ciphertext bytea, status text NOT NULL, subscriptions text[] NOT NULL);
 ALTER TABLE webhook_config ADD COLUMN IF NOT EXISTS secret_ciphertext bytea;
 CREATE TABLE IF NOT EXISTS outbound_events (event_id text PRIMARY KEY, institution_id text NOT NULL, event_type text NOT NULL, correlation_id text NOT NULL, aggregate_id text NOT NULL, body jsonb NOT NULL, status text NOT NULL DEFAULT 'PENDING', attempt_count int NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(), occurred_at timestamptz NOT NULL);
 CREATE TABLE IF NOT EXISTS webhook_delivery_attempts (event_id text NOT NULL REFERENCES outbound_events(event_id), attempt_number int NOT NULL, attempted_at timestamptz NOT NULL DEFAULT now(), http_status int NOT NULL DEFAULT 0, outcome text NOT NULL, completed_at timestamptz, PRIMARY KEY(event_id,attempt_number));
 ALTER TABLE outbound_events ADD COLUMN IF NOT EXISTS round_attempt_count int NOT NULL DEFAULT 0;
 ALTER TABLE outbound_events ADD COLUMN IF NOT EXISTS replay_count int NOT NULL DEFAULT 0;
 CREATE TABLE IF NOT EXISTS webhook_delivery_replays (institution_id text NOT NULL,event_id text NOT NULL REFERENCES outbound_events(event_id),idempotency_key text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(institution_id,idempotency_key));`)
	return err
}
func (p *Postgres) Configure(institution, endpointURL, signingSecret string, subscriptions []string) error {
	if institution == "" || endpointURL == "" || signingSecret == "" || len(subscriptions) == 0 {
		return errors.New("webhook configuration incomplete")
	}
	if err := validateCustomerWebhookURL(context.Background(), endpointURL); err != nil {
		return err
	}
	ciphertext, err := p.encryptSecret(signingSecret)
	if err != nil {
		return err
	}
	_, err = p.DB.Exec(`INSERT INTO webhook_config(institution_id,endpoint_url,secret_ref,secret_ciphertext,status,subscriptions) VALUES($1,$2,'encrypted',$3,'ACTIVE',$4) ON CONFLICT(institution_id) DO UPDATE SET endpoint_url=excluded.endpoint_url,secret_ref=excluded.secret_ref,secret_ciphertext=excluded.secret_ciphertext,status=excluded.status,subscriptions=excluded.subscriptions`, institution, endpointURL, ciphertext, subscriptions)
	return err
}

func (p *Postgres) encryptSecret(secret string) ([]byte, error) {
	if len(p.ConfigEncryptionKey) < 32 {
		return nil, errors.New("webhook configuration encryption key must contain at least 32 bytes")
	}
	key := sha256.Sum256(p.ConfigEncryptionKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, []byte(secret), nil)...), nil
}

func (p *Postgres) decryptSecret(ciphertext []byte) (string, error) {
	if len(p.ConfigEncryptionKey) < 32 {
		return "", errors.New("webhook configuration encryption key must contain at least 32 bytes")
	}
	key := sha256.Sum256(p.ConfigEncryptionKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", errors.New("webhook configuration ciphertext is invalid")
	}
	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt webhook configuration: %w", err)
	}
	return string(plaintext), nil
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
	if prior.EventId != event.EventId || prior.InstitutionId != event.InstitutionId || prior.EventType != event.EventType ||
		prior.CorrelationId != event.CorrelationId || prior.AggregateId != event.AggregateId ||
		prior.OccurredAt != event.OccurredAt || prior.PayloadJson != event.PayloadJson {
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
	var id, inst, eventType, endpoint string
	var body []byte
	var secretCiphertext []byte
	var attempts, roundAttempts int
	err = tx.QueryRowContext(ctx, `SELECT e.event_id,e.institution_id,e.event_type,e.body,e.attempt_count,e.round_attempt_count,c.endpoint_url,c.secret_ciphertext FROM outbound_events e JOIN webhook_config c ON c.institution_id=e.institution_id AND c.status='ACTIVE' AND e.event_type=ANY(c.subscriptions) WHERE e.status IN ('PENDING','RETRY') AND e.next_attempt_at<=now() ORDER BY e.next_attempt_at,e.event_id LIMIT 1 FOR UPDATE OF e SKIP LOCKED`).Scan(&id, &inst, &eventType, &body, &attempts, &roundAttempts, &endpoint, &secretCiphertext)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	secret, err := p.decryptSecret(secretCiphertext)
	if err != nil {
		return false, err
	}
	var ev notificationv1.Event
	if err = json.Unmarshal(body, &ev); err != nil {
		return false, err
	}
	attempt, deliveryErr := DeliverCustomerEvent(p.Client, CustomerWebhookConfig{InstitutionID: inst, EndpointURL: endpoint, Secret: secret}, OutboundEvent{EventID: id, InstitutionID: inst, EventType: eventType, EventVersion: "v1", CorrelationID: ev.CorrelationId, Payload: ev.PayloadJson, OccurredAt: parseTime(ev.OccurredAt)})
	if attempt == nil {
		now := time.Now().UTC()
		attempt = &DeliveryAttempt{
			EventID:       id,
			InstitutionID: inst,
			AttemptedAt:   now,
			CompletedAt:   now,
			Outcome:       "failed",
		}
	}
	number := attempts + 1
	roundNumber := roundAttempts + 1
	max := p.MaxAttempts
	if max < 1 {
		max = 3
	}
	delay := p.RetryDelay
	if delay <= 0 {
		delay = time.Minute
	}
	next := time.Now().Add(delay * time.Duration(1<<min(roundAttempts, 8)))
	statusValue := "RETRY"
	if attempt.Outcome == "success" {
		statusValue = "SUCCESS"
	} else if roundNumber >= max || attempt.Outcome == "failed" {
		statusValue = "DEAD_LETTER"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_delivery_attempts(event_id,attempt_number,http_status,outcome,completed_at) VALUES($1,$2,$3,$4,now())`, id, number, attempt.HTTPStatus, attempt.Outcome); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_events SET status=$2,attempt_count=$3,round_attempt_count=$4,next_attempt_at=$5 WHERE event_id=$1`, id, statusValue, number, roundNumber, next); err != nil {
		return false, err
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}
	if deliveryErr != nil || attempt.Outcome != "success" {
		return true, errors.New("customer webhook delivery attempt failed; result persisted")
	}
	return true, nil
}

func (p *Postgres) Replay(ctx context.Context, institutionID, eventID, idempotencyKey string) error {
	if institutionID == "" || eventID == "" || idempotencyKey == "" {
		return errors.New("institution, event, and idempotency key required")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var priorEventID string
	err = tx.QueryRowContext(ctx, `SELECT event_id FROM webhook_delivery_replays WHERE institution_id=$1 AND idempotency_key=$2`, institutionID, idempotencyKey).Scan(&priorEventID)
	if err == nil {
		if priorEventID != eventID {
			return ErrReplayKeyConflict
		}
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}

	var status string
	var replayCount int
	err = tx.QueryRowContext(ctx, `SELECT status,replay_count FROM outbound_events WHERE event_id=$1 AND institution_id=$2 FOR UPDATE`, eventID, institutionID).Scan(&status, &replayCount)
	if err == sql.ErrNoRows {
		return ErrReplayEventNotFound
	}
	if err != nil {
		return err
	}
	if status != "SUCCESS" && status != "DEAD_LETTER" && !((status == "PENDING" || status == "RETRY") && replayCount > 0) {
		return ErrReplayNotAllowed
	}
	var configured bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_config c JOIN outbound_events e ON e.institution_id=c.institution_id WHERE e.event_id=$1 AND c.institution_id=$2 AND c.status='ACTIVE' AND e.event_type=ANY(c.subscriptions))`, eventID, institutionID).Scan(&configured); err != nil {
		return err
	}
	if !configured {
		return ErrReplayNotAllowed
	}
	if status == "SUCCESS" || status == "DEAD_LETTER" {
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_events SET status='PENDING',round_attempt_count=0,replay_count=replay_count+1,next_attempt_at=now() WHERE event_id=$1 AND institution_id=$2`, eventID, institutionID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO webhook_delivery_replays(institution_id,event_id,idempotency_key) VALUES($1,$2,$3) ON CONFLICT(institution_id,idempotency_key) DO NOTHING`, institutionID, eventID, idempotencyKey)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT event_id FROM webhook_delivery_replays WHERE institution_id=$1 AND idempotency_key=$2`, institutionID, idempotencyKey).Scan(&priorEventID); err != nil {
			return err
		}
		if priorEventID != eventID {
			return ErrReplayKeyConflict
		}
	}
	return tx.Commit()
}

func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func (p *Postgres) EventStatus(id string) (string, int, error) {
	var status string
	var count int
	err := p.DB.QueryRow(`SELECT status,attempt_count FROM outbound_events WHERE event_id=$1`, id).Scan(&status, &count)
	return status, count, err
}
func (p *Postgres) String() string { return fmt.Sprintf("notification database ready") }
