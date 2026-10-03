package flutterwave

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresCreateClaimSurvivesRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL DSN not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skip("PostgreSQL unavailable")
	}
	store := PostgresAttempts{DB: db}
	if err := store.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	ref := "si_claim_" + time.Now().UTC().Format("20060102150405.000000000")
	first, err := store.Claim(context.Background(), ref, "hash-one")
	if err != nil || !first {
		t.Fatalf("first claim: %v %v", first, err)
	}
	restarted := PostgresAttempts{DB: db}
	again, err := restarted.Claim(context.Background(), ref, "hash-one")
	if err != nil || again {
		t.Fatalf("restarted claim: %v %v", again, err)
	}
	if _, err := restarted.Claim(context.Background(), ref, "hash-two"); err == nil {
		t.Fatal("different economics reused reference")
	}
}
