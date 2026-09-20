package settlementstore

import "database/sql"

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS settlement_instructions (id TEXT PRIMARY KEY, client_ref TEXT NOT NULL UNIQUE, provider_ref TEXT NOT NULL DEFAULT '', beneficiary TEXT NOT NULL, amount_minor BIGINT NOT NULL CHECK (amount_minor > 0), currency TEXT NOT NULL, purpose TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, supersedes TEXT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL); CREATE TABLE IF NOT EXISTS settlement_transitions (id BIGSERIAL PRIMARY KEY, settlement_id TEXT NOT NULL REFERENCES settlement_instructions(id), from_state TEXT NOT NULL, to_state TEXT NOT NULL, event TEXT NOT NULL, reason TEXT NOT NULL, source TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL); CREATE TABLE IF NOT EXISTS settlement_evidence (evidence_id TEXT PRIMARY KEY, settlement_id TEXT NOT NULL REFERENCES settlement_instructions(id), provider TEXT NOT NULL, method TEXT NOT NULL, request_ref TEXT NOT NULL, raw_payload_hash TEXT NOT NULL, parsed_at TIMESTAMPTZ NOT NULL, correlation_id TEXT NOT NULL);`)
	return err
}
