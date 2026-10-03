package webhookcore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/provider"
)

type ProviderInbox interface {
	Accept(context.Context, string, provider.WebhookEvent, func() error) (bool, error)
}

type PostgresProviderInbox struct{ DB *sql.DB }

func (p PostgresProviderInbox) EnsureSchema() error {
	if p.DB == nil {
		return errors.New("provider inbox database required")
	}
	_, err := p.DB.Exec(`CREATE TABLE IF NOT EXISTS provider_webhook_inbox (
		provider text NOT NULL,
		provider_event_id text NOT NULL,
		client_ref text NOT NULL,
		raw_payload_hash text NOT NULL,
		accepted_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY(provider,provider_event_id)
	)`)
	return err
}

// The advisory lock serializes delivery across service processes. A signal is
// acknowledged only after its receipt is durably committed.
func (p PostgresProviderInbox) Accept(ctx context.Context, providerName string, event provider.WebhookEvent, signal func() error) (bool, error) {
	if p.DB == nil {
		return false, errors.New("provider inbox database required")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = postgres.Lock(tx, "provider-webhook:"+providerName+":"+event.ProviderEventID); err != nil {
		return false, err
	}
	var clientRef, payloadHash string
	err = tx.QueryRowContext(ctx, `SELECT client_ref,raw_payload_hash FROM provider_webhook_inbox WHERE provider=$1 AND provider_event_id=$2`, providerName, event.ProviderEventID).Scan(&clientRef, &payloadHash)
	if err == nil {
		if clientRef != event.ClientRef || payloadHash != event.Evidence.RawPayloadHash {
			return false, errors.New("provider webhook event collision")
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err = signal(); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_webhook_inbox(provider,provider_event_id,client_ref,raw_payload_hash) VALUES($1,$2,$3,$4)`, providerName, event.ProviderEventID, event.ClientRef, event.Evidence.RawPayloadHash)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}
