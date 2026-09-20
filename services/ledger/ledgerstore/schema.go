package ledgerstore

import "database/sql"

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ledger_journals (dedup_key TEXT PRIMARY KEY, journal_id TEXT NOT NULL UNIQUE, trade_id TEXT NOT NULL, currency TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()); CREATE TABLE IF NOT EXISTS ledger_entries (id BIGSERIAL PRIMARY KEY, journal_id TEXT NOT NULL REFERENCES ledger_journals(journal_id), account_code TEXT NOT NULL, account_name TEXT NOT NULL, owner TEXT NOT NULL, currency TEXT NOT NULL, debit BIGINT NOT NULL, credit BIGINT NOT NULL, external_ref TEXT NOT NULL);`)
	return err
}
