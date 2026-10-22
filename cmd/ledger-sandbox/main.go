// ledger-sandbox replays explicit accounting fixtures into a dedicated local
// proof database. It never contacts a provider or enables a production route.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/services/ledger/ledgerstore"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox accounting replay failed:", err)
		os.Exit(1)
	}
}
func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ledger-sandbox", flag.ContinueOnError)
	manifest := flags.String("manifest", "", "JSON array of test-only accounting commands")
	proof := flags.Bool("local-proof", false, "acknowledge fixture-only accounting; no real money")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*proof || *manifest == "" || flags.NArg() != 0 {
		return errors.New("--local-proof and --manifest are required")
	}
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if !localProofDSN(dsn) {
		return errors.New("loopback PostgreSQL URL and osai_proof_ database required")
	}
	file, err := os.Open(*manifest)
	if err != nil {
		return errors.New("cannot open fixture manifest")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	var commands []ledgerstore.SandboxAccountingCommand
	if err = decoder.Decode(&commands); err != nil {
		return errors.New("invalid fixture manifest")
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected manifest content")
	}
	if len(commands) == 0 || len(commands) > 1000 {
		return errors.New("fixture manifest requires 1 to 1000 commands")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("cannot open local proof database")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := ledgerstore.NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		return errors.New("cannot initialise local sandbox accounting")
	}
	encoder := json.NewEncoder(output)
	for _, command := range commands {
		state, replay, err := store.Apply(ctx, command)
		if err != nil {
			return fmt.Errorf("event %q: %w", command.EventID, err)
		}
		if err = encoder.Encode(map[string]any{"scope": "test-only", "event_id": command.EventID, "state": state, "replay": replay}); err != nil {
			return err
		}
	}
	return nil
}
func localProofDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil || u == nil {
		return false
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return false
	}
	if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return false
	}
	if !strings.HasPrefix(u.Path, "/osai_proof_") || u.Fragment != "" {
		return false
	}
	// libpq query options must not override the inspected host/database.
	allowed := map[string]bool{"sslmode": true, "connect_timeout": true, "search_path": true, "application_name": true}
	for key := range u.Query() {
		if !allowed[key] {
			return false
		}
	}
	return true
}
