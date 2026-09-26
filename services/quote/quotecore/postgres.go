package quotecore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/postgres"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"google.golang.org/grpc/metadata"
	"time"
)

func NewPostgresStore(db *sql.DB) (*QuoteStore, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS quotes (id text PRIMARY KEY, institution_id text NOT NULL, idempotency_key text NOT NULL, body jsonb NOT NULL, UNIQUE(institution_id,idempotency_key));
 CREATE TABLE IF NOT EXISTS quote_outbox (event_id text PRIMARY KEY, institution_id text NOT NULL, body jsonb NOT NULL, delivered_at timestamptz);`)
	if err != nil {
		return nil, err
	}
	s := NewQuoteStore()
	s.db = db
	return s, nil
}
func (s *QuoteStore) createDB(q *ExecutableQuote) (*ExecutableQuote, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = postgres.Lock(tx, "quote-create:"+q.Request.CustomerID+":"+q.Request.IdempotencyKey); err != nil {
		return nil, err
	}
	var raw []byte
	err = tx.QueryRow(`SELECT body FROM quotes WHERE institution_id=$1 AND idempotency_key=$2`, q.Request.CustomerID, q.Request.IdempotencyKey).Scan(&raw)
	if err == nil {
		var old ExecutableQuote
		if err = json.Unmarshal(raw, &old); err != nil {
			return nil, err
		}
		if old.RequestHash != q.RequestHash {
			return nil, errors.New("idempotency conflict")
		}
		return &old, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	raw, err = json.Marshal(q)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO quotes(id,institution_id,idempotency_key,body) VALUES($1,$2,$3,$4)`, q.ID, q.Request.CustomerID, q.Request.IdempotencyKey, string(raw))
	if err != nil {
		return nil, err
	}
	return q, tx.Commit()
}
func (s *QuoteStore) getDB(id string) (*ExecutableQuote, bool) {
	var raw []byte
	if s.db.QueryRow(`SELECT body FROM quotes WHERE id=$1`, id).Scan(&raw) != nil {
		return nil, false
	}
	var q ExecutableQuote
	if json.Unmarshal(raw, &q) != nil {
		return nil, false
	}
	return &q, true
}
func (s *QuoteStore) acceptDB(id, key, tradeID, correlation string, now time.Time) (*ExecutableQuote, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM quotes WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var q ExecutableQuote
	if err = json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	if q.Status == QuoteStatusAccepted {
		if q.AcceptedTradeID != tradeID {
			return nil, errors.New("quote already accepted")
		}
		return &q, tx.Commit()
	}
	if q.Status != QuoteStatusQuoted || !now.Before(q.ExpiresAt) {
		return nil, errors.New("quote is not executable")
	}
	q.Status = QuoteStatusAccepted
	q.AcceptedTradeID = tradeID
	q.AcceptanceKey = key
	q.AcceptedAt = &now
	raw, err = json.Marshal(q)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE quotes SET body=$2 WHERE id=$1`, id, string(raw)); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"quote_id": id, "trade_id": tradeID, "status": "ACCEPTED"})
	event := notificationv1.Event{EventId: "evt_quote_accepted_" + id, InstitutionId: q.Request.CustomerID, EventType: "quote.accepted", CorrelationId: correlation, AggregateId: id, OccurredAt: now.UTC().Format(time.RFC3339Nano), PayloadJson: string(payload)}
	raw, err = json.Marshal(&event)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO quote_outbox(event_id,institution_id,body) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, event.EventId, event.InstitutionId, string(raw)); err != nil {
		return nil, err
	}
	return &q, tx.Commit()
}
func (s *QuoteStore) AcceptWithCorrelation(id, key, tradeID, correlation string, now time.Time) (*ExecutableQuote, error) {
	if s.db != nil {
		return s.acceptDB(id, key, tradeID, correlation, now)
	}
	return s.Accept(id, key, tradeID, now)
}

// Relay retries until Notification has durably acknowledged the same event ID.
func (s *QuoteStore) Relay(ctx context.Context, client notificationv1.NotificationServiceClient, token string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,body FROM quote_outbox WHERE delivered_at IS NULL ORDER BY event_id LIMIT 50`)
	if err != nil {
		return err
	}
	type pending struct {
		id  string
		raw []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.id, &p.raw); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range batch {
		var ev notificationv1.Event
		if err = json.Unmarshal(p.raw, &ev); err != nil {
			return err
		}
		call, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), 5*time.Second)
		_, err = client.PublishEvent(call, &ev)
		cancel()
		if err != nil {
			return err
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE quote_outbox SET delivered_at=now() WHERE event_id=$1`, p.id); err != nil {
			return err
		}
	}
	return nil
}
