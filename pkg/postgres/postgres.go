// Package postgres provides connection and transaction primitives only. Each
// service defines and accesses its own tables.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
	"strings"
	"time"
)

func Open(service string) (*sql.DB, error) {
	dsn := os.Getenv("OSAI_" + strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("OSAI_POSTGRES_DSN")
	}
	if dsn == "" {
		return nil, errors.New("PostgreSQL configuration required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	db.SetMaxOpenConns(10)
	return db, nil
}

// Lock serializes a domain operation across service instances, including the
// first insert when no row exists yet. The lock is released by transaction end.
func Lock(tx *sql.Tx, key string) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
	return err
}
