CREATE TABLE IF NOT EXISTS reconciliation_evidence (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  source TEXT NOT NULL,
  account TEXT NOT NULL,
  raw_payload_hash TEXT NOT NULL,
  ingested_at TIMESTAMPTZ NOT NULL,
  watermark TEXT NOT NULL DEFAULT '',
  paging_token TEXT NOT NULL DEFAULT '',
  amount_minor BIGINT NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  client_ref TEXT NOT NULL DEFAULT '',
  provider_ref TEXT NOT NULL DEFAULT '',
  beneficiary TEXT NOT NULL DEFAULT '',
  fee_minor BIGINT NOT NULL DEFAULT 0,
  correlation_id TEXT NOT NULL DEFAULT '',
  raw_payload JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS reconciliation_breaks (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  provider TEXT NOT NULL,
  currency TEXT NOT NULL,
  expected_value BIGINT NOT NULL DEFAULT 0,
  observed_value BIGINT NOT NULL DEFAULT 0,
  delta BIGINT NOT NULL DEFAULT 0,
  value BIGINT NOT NULL DEFAULT 0,
  age_seconds BIGINT NOT NULL DEFAULT 0,
  owner TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'OPEN',
  severity TEXT NOT NULL DEFAULT 'medium',
  evidence_ids TEXT[] NOT NULL DEFAULT '{}',
  internal_ids TEXT[] NOT NULL DEFAULT '{}',
  comments TEXT[] NOT NULL DEFAULT '{}',
  resolution TEXT NOT NULL DEFAULT '',
  correlation_id TEXT NOT NULL DEFAULT '',
  history JSONB NOT NULL DEFAULT '[]',
  first_seen TIMESTAMPTZ,
  last_seen TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS reconciliation_match_results (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL DEFAULT '',
  evidence_id TEXT NOT NULL DEFAULT '',
  internal_id TEXT NOT NULL DEFAULT '',
  strategy TEXT NOT NULL DEFAULT 'exact',
  matched BOOLEAN NOT NULL DEFAULT false,
  break_id TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS reconciliation_watermarks (
  provider TEXT NOT NULL,
  account TEXT NOT NULL,
  currency TEXT NOT NULL,
  watermark TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, account, currency)
);

CREATE TABLE IF NOT EXISTS reconciliation_approvals (
  id TEXT PRIMARY KEY,
  break_id TEXT NOT NULL,
  proposer TEXT NOT NULL,
  approver TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  evidence_ids TEXT[] NOT NULL DEFAULT '{}',
  proposed_ledger_effect JSONB NOT NULL DEFAULT '{}',
  resulting_journal_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'PROPOSED',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS reconciliation_resolution_history (
  id TEXT PRIMARY KEY,
  break_id TEXT NOT NULL,
  action TEXT NOT NULL,
  actor TEXT NOT NULL,
  details JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
