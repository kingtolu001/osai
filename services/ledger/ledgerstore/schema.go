package ledgerstore

import "database/sql"

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ledger_journals (dedup_key TEXT PRIMARY KEY, journal_id TEXT NOT NULL UNIQUE, trade_id TEXT NOT NULL, institution_id TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()); ALTER TABLE ledger_journals ADD COLUMN IF NOT EXISTS institution_id TEXT NOT NULL DEFAULT ''; ALTER TABLE ledger_journals ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'legacy'; CREATE TABLE IF NOT EXISTS ledger_entries (id BIGSERIAL PRIMARY KEY, journal_id TEXT NOT NULL REFERENCES ledger_journals(journal_id), account_code TEXT NOT NULL, account_name TEXT NOT NULL, owner TEXT NOT NULL, currency TEXT NOT NULL, debit BIGINT NOT NULL, credit BIGINT NOT NULL, external_ref TEXT NOT NULL);`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS ledger_prefund_evidence (
		evidence_reference text PRIMARY KEY, institution_id text NOT NULL, currency text NOT NULL,
		amount_minor bigint NOT NULL CHECK (amount_minor > 0), journal_id text NOT NULL UNIQUE REFERENCES ledger_journals(journal_id));
	CREATE TABLE IF NOT EXISTS ledger_funding_reservations (
		trade_id text PRIMARY KEY, institution_id text NOT NULL, currency text NOT NULL,
		amount_minor bigint NOT NULL CHECK (amount_minor > 0), state text NOT NULL CHECK (state IN ('ACTIVE','CONSUMED','RELEASED')),
		journal_id text NOT NULL UNIQUE REFERENCES ledger_journals(journal_id),
		settlement_id text NOT NULL DEFAULT '', settlement_ref text NOT NULL DEFAULT '',
		source_principal_minor bigint NOT NULL DEFAULT 0, fee_minor bigint NOT NULL DEFAULT 0,
		pricing_hash text NOT NULL DEFAULT '', pricing_snapshot_json text NOT NULL DEFAULT '');
	ALTER TABLE ledger_funding_reservations ADD COLUMN IF NOT EXISTS source_principal_minor bigint NOT NULL DEFAULT 0;
	ALTER TABLE ledger_funding_reservations ADD COLUMN IF NOT EXISTS fee_minor bigint NOT NULL DEFAULT 0;
	ALTER TABLE ledger_funding_reservations ADD COLUMN IF NOT EXISTS accounting_policy text NOT NULL DEFAULT '';
	ALTER TABLE ledger_funding_reservations ADD COLUMN IF NOT EXISTS pricing_hash text NOT NULL DEFAULT '';
	ALTER TABLE ledger_funding_reservations ADD COLUMN IF NOT EXISTS pricing_snapshot_json text NOT NULL DEFAULT '';
	CREATE TABLE IF NOT EXISTS ledger_fee_finality (trade_id text PRIMARY KEY REFERENCES ledger_funding_reservations(trade_id), policy_version text NOT NULL, command_hash text NOT NULL, correlation_id text NOT NULL, provider_ref text NOT NULL, service_actor text NOT NULL, reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
	UPDATE ledger_funding_reservations SET source_principal_minor=amount_minor
		WHERE source_principal_minor=0 AND fee_minor=0 AND pricing_hash='';`)
	return err
}
