package flutterwave

import (
	"context"
	"database/sql"
	"errors"

	"github.com/osai/osai/pkg/postgres"
)

// AttemptStore records the only permitted create attempt before the HTTP call.
// A replay must query Flutterwave by the same reference instead of posting again.
type AttemptStore interface {
	Claim(ctx context.Context, clientRef, requestHash string) (bool, error)
	RecordProviderRef(ctx context.Context, clientRef, providerRef string) error
}

type PostgresAttempts struct{ DB *sql.DB }

func (p PostgresAttempts) EnsureSchema() error {
	if p.DB == nil {
		return errors.New("Flutterwave attempt database required")
	}
	_, err := p.DB.Exec(`CREATE TABLE IF NOT EXISTS flutterwave_transfer_attempts (
		client_ref text PRIMARY KEY,
		request_hash text NOT NULL,
		provider_ref text NOT NULL DEFAULT '',
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

func (p PostgresAttempts) Claim(ctx context.Context, clientRef, requestHash string) (bool, error) {
	if p.DB == nil {
		return false, errors.New("Flutterwave attempt database required")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = postgres.Lock(tx, "flutterwave-create:"+clientRef); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO flutterwave_transfer_attempts(client_ref,request_hash) VALUES($1,$2) ON CONFLICT(client_ref) DO NOTHING`, clientRef, requestHash)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	var existing string
	if err = tx.QueryRowContext(ctx, `SELECT request_hash FROM flutterwave_transfer_attempts WHERE client_ref=$1`, clientRef).Scan(&existing); err != nil {
		return false, err
	}
	if existing != requestHash {
		return false, errors.New("Flutterwave reference reused with different transfer details")
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return inserted == 1, nil
}

func (p PostgresAttempts) RecordProviderRef(ctx context.Context, clientRef, providerRef string) error {
	if p.DB == nil {
		return errors.New("Flutterwave attempt database required")
	}
	result, err := p.DB.ExecContext(ctx, `UPDATE flutterwave_transfer_attempts SET provider_ref=$2,updated_at=now() WHERE client_ref=$1 AND (provider_ref='' OR provider_ref=$2)`, clientRef, providerRef)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("Flutterwave attempt provider reference conflict")
	}
	return nil
}
