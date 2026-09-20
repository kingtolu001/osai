package reconciliation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Store persists reconciliation runtime state without allowing any balance-writing path.
type Store interface {
	SaveEvidence(Evidence) error
	LoadEvidence() ([]Evidence, error)
	SaveBreak(*ReconciliationBreak) error
	LoadBreaks() ([]*ReconciliationBreak, error)
	SaveMatchResult(MatchResult) error
	LoadMatchResults() ([]MatchResult, error)
	SaveWatermark(Watermark) error
	LoadWatermarks() ([]Watermark, error)
	SaveApproval(*Approval) error
	LoadApprovals() ([]*Approval, error)
	SaveResolution(*ResolutionEvent) error
	LoadResolutionHistory() ([]*ResolutionEvent, error)
}

type MemoryStore struct {
	mu          sync.Mutex
	evidence    map[string]Evidence
	breaks      map[string]*ReconciliationBreak
	matchs      map[string]MatchResult
	watermarks  map[string]Watermark
	approvals   map[string]*Approval
	resolutions map[string]*ResolutionEvent
}

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS reconciliation_evidence (id TEXT PRIMARY KEY, provider TEXT NOT NULL, source TEXT NOT NULL, account TEXT NOT NULL, raw_payload_hash TEXT NOT NULL, ingested_at TIMESTAMPTZ NOT NULL, watermark TEXT NOT NULL DEFAULT '', paging_token TEXT NOT NULL DEFAULT '', amount_minor BIGINT NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT 'USD', client_ref TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '', beneficiary TEXT NOT NULL DEFAULT '', fee_minor BIGINT NOT NULL DEFAULT 0, correlation_id TEXT NOT NULL DEFAULT '', raw_payload JSONB NOT NULL DEFAULT '{}'); CREATE TABLE IF NOT EXISTS reconciliation_breaks (id TEXT PRIMARY KEY, type TEXT NOT NULL, provider TEXT NOT NULL, currency TEXT NOT NULL, expected_value BIGINT NOT NULL DEFAULT 0, observed_value BIGINT NOT NULL DEFAULT 0, delta BIGINT NOT NULL DEFAULT 0, value BIGINT NOT NULL DEFAULT 0, age_seconds BIGINT NOT NULL DEFAULT 0, owner TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'OPEN', severity TEXT NOT NULL DEFAULT 'medium', evidence_ids TEXT[] NOT NULL DEFAULT '{}', internal_ids TEXT[] NOT NULL DEFAULT '{}', comments TEXT[] NOT NULL DEFAULT '{}', resolution TEXT NOT NULL DEFAULT '', correlation_id TEXT NOT NULL DEFAULT '', history JSONB NOT NULL DEFAULT '[]', first_seen TIMESTAMPTZ, last_seen TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now()); CREATE TABLE IF NOT EXISTS reconciliation_match_results (id TEXT PRIMARY KEY, run_id TEXT NOT NULL DEFAULT '', evidence_id TEXT NOT NULL DEFAULT '', internal_id TEXT NOT NULL DEFAULT '', strategy TEXT NOT NULL DEFAULT 'exact', matched BOOLEAN NOT NULL DEFAULT false, break_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now()); CREATE TABLE IF NOT EXISTS reconciliation_watermarks (provider TEXT NOT NULL, account TEXT NOT NULL, currency TEXT NOT NULL, watermark TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (provider, account, currency)); CREATE TABLE IF NOT EXISTS reconciliation_approvals (id TEXT PRIMARY KEY, break_id TEXT NOT NULL, proposer TEXT NOT NULL, approver TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', evidence_ids TEXT[] NOT NULL DEFAULT '{}', proposed_ledger_effect JSONB NOT NULL DEFAULT '{}', resulting_journal_ref TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'PROPOSED', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()); CREATE TABLE IF NOT EXISTS reconciliation_resolution_history (id TEXT PRIMARY KEY, break_id TEXT NOT NULL, action TEXT NOT NULL, actor TEXT NOT NULL, details JSONB NOT NULL DEFAULT '{}', created_at TIMESTAMPTZ NOT NULL DEFAULT now()); ALTER TABLE reconciliation_breaks ADD COLUMN IF NOT EXISTS history JSONB NOT NULL DEFAULT '[]';`)
	return err
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		evidence:    make(map[string]Evidence),
		breaks:      make(map[string]*ReconciliationBreak),
		matchs:      make(map[string]MatchResult),
		watermarks:  make(map[string]Watermark),
		approvals:   make(map[string]*Approval),
		resolutions: make(map[string]*ResolutionEvent),
	}
}

func (s *MemoryStore) SaveEvidence(ev Evidence) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if ev.ID == "" { ev.ID = fmt.Sprintf("ev_%d", time.Now().UnixNano()) }
	s.evidence[ev.ID] = ev
	return nil
}

func (s *MemoryStore) LoadEvidence() ([]Evidence, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]Evidence, 0, len(s.evidence))
	for _, ev := range s.evidence { items = append(items, ev) }
	return items, nil
}

func (s *MemoryStore) SaveBreak(brk *ReconciliationBreak) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if brk == nil { return nil }
	if brk.ID == "" { brk.ID = fmt.Sprintf("break_%d", time.Now().UnixNano()) }
	s.breaks[brk.ID] = brk
	return nil
}

func (s *MemoryStore) LoadBreaks() ([]*ReconciliationBreak, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]*ReconciliationBreak, 0, len(s.breaks))
	for _, brk := range s.breaks { items = append(items, brk) }
	return items, nil
}

func (s *MemoryStore) SaveMatchResult(match MatchResult) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if match.ID == "" { match.ID = fmt.Sprintf("match_%d", time.Now().UnixNano()) }
	s.matchs[match.ID] = match
	return nil
}

func (s *MemoryStore) LoadMatchResults() ([]MatchResult, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]MatchResult, 0, len(s.matchs))
	for _, m := range s.matchs { items = append(items, m) }
	return items, nil
}

func (s *MemoryStore) SaveWatermark(w Watermark) error {
	s.mu.Lock(); defer s.mu.Unlock()
	key := w.Provider + "|" + w.Account + "|" + w.Currency
	w.UpdatedAt = time.Now().UTC()
	s.watermarks[key] = w
	return nil
}

func (s *MemoryStore) LoadWatermarks() ([]Watermark, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]Watermark, 0, len(s.watermarks))
	for _, w := range s.watermarks { items = append(items, w) }
	return items, nil
}

func (s *MemoryStore) SaveApproval(app *Approval) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if app == nil { return nil }
	if app.ID == "" { app.ID = fmt.Sprintf("approval_%d", time.Now().UnixNano()) }
	s.approvals[app.ID] = app
	return nil
}

func (s *MemoryStore) LoadApprovals() ([]*Approval, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]*Approval, 0, len(s.approvals))
	for _, a := range s.approvals { items = append(items, a) }
	return items, nil
}

func (s *MemoryStore) SaveResolution(res *ResolutionEvent) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if res == nil { return nil }
	if res.ID == "" { res.ID = fmt.Sprintf("res_%d", time.Now().UnixNano()) }
	s.resolutions[res.ID] = res
	return nil
}

func (s *MemoryStore) LoadResolutionHistory() ([]*ResolutionEvent, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]*ResolutionEvent, 0, len(s.resolutions))
	for _, r := range s.resolutions { items = append(items, r) }
	return items, nil
}

// PostgresStore persists reconciliation state in PostgreSQL.
type PostgresStore struct { DB *sql.DB }

func (p *PostgresStore) SaveEvidence(ev Evidence) error {
	if p == nil || p.DB == nil { return nil }
	_, err := p.DB.Exec(`INSERT INTO reconciliation_evidence (id, provider, source, account, raw_payload_hash, ingested_at, watermark, paging_token, amount_minor, currency, client_ref, provider_ref, beneficiary, fee_minor, correlation_id, raw_payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (id) DO NOTHING`, ev.ID, ev.Provider, ev.Source, ev.Account, ev.RawPayloadHash, ev.IngestedAt, ev.Watermark, ev.PagingToken, ev.AmountMinor, ev.Currency, ev.ClientRef, ev.ProviderRef, ev.Beneficiary, ev.FeeMinor, ev.CorrelationID, marshalJSON(ev.RawPayload))
	return err
}

func (p *PostgresStore) LoadEvidence() ([]Evidence, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT id, provider, source, account, raw_payload_hash, ingested_at, watermark, paging_token, amount_minor, currency, client_ref, provider_ref, beneficiary, fee_minor, correlation_id FROM reconciliation_evidence ORDER BY ingested_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]Evidence, 0)
	for rows.Next() {
		var ev Evidence
		if err := rows.Scan(&ev.ID, &ev.Provider, &ev.Source, &ev.Account, &ev.RawPayloadHash, &ev.IngestedAt, &ev.Watermark, &ev.PagingToken, &ev.AmountMinor, &ev.Currency, &ev.ClientRef, &ev.ProviderRef, &ev.Beneficiary, &ev.FeeMinor, &ev.CorrelationID); err != nil { return nil, err }
		items = append(items, ev)
	}
	return items, rows.Err()
}

func (p *PostgresStore) SaveBreak(brk *ReconciliationBreak) error {
	if p == nil || p.DB == nil || brk == nil { return nil }
	historyJSON, _ := json.Marshal(brk.History)
	_, err := p.DB.Exec(`INSERT INTO reconciliation_breaks (id, type, provider, currency, expected_value, observed_value, delta, value, age_seconds, owner, state, severity, evidence_ids, internal_ids, comments, resolution, correlation_id, history, first_seen, last_seen, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state, severity=EXCLUDED.severity, observed_value=EXCLUDED.observed_value, delta=EXCLUDED.delta, last_seen=EXCLUDED.last_seen, resolution=EXCLUDED.resolution, history=EXCLUDED.history`, brk.ID, string(brk.Type), brk.Provider, brk.Currency, brk.Expected, brk.Observed, brk.Delta, brk.Value, int64(brk.Age.Seconds()), brk.Owner, brk.State, brk.Severity, pqArray(brk.EvidenceIDs), pqArray(brk.InternalIDs), pqArray(brk.Comments), brk.Resolution, brk.CorrelationID, historyJSON, brk.FirstSeen, brk.LastSeen, brk.CreatedAt)
	return err
}

func (p *PostgresStore) LoadBreaks() ([]*ReconciliationBreak, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT id, type, provider, currency, expected_value, observed_value, delta, value, age_seconds, owner, state, severity, evidence_ids, internal_ids, comments, resolution, correlation_id, history, first_seen, last_seen, created_at FROM reconciliation_breaks ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]*ReconciliationBreak, 0)
	for rows.Next() {
		var b ReconciliationBreak
		var evidenceIDs, internalIDs, comments, historyJSON sql.NullString
		if err := rows.Scan(&b.ID, &b.Type, &b.Provider, &b.Currency, &b.Expected, &b.Observed, &b.Delta, &b.Value, &b.AgeSeconds, &b.Owner, &b.State, &b.Severity, &evidenceIDs, &internalIDs, &comments, &b.Resolution, &b.CorrelationID, &historyJSON, &b.FirstSeen, &b.LastSeen, &b.CreatedAt); err != nil { return nil, err }
		b.EvidenceIDs = parseStringList(evidenceIDs)
		b.InternalIDs = parseStringList(internalIDs)
		b.Comments = parseStringList(comments)
		b.Age = time.Duration(b.AgeSeconds) * time.Second
		if historyJSON.Valid && historyJSON.String != "" && historyJSON.String != "null" {
			json.Unmarshal([]byte(historyJSON.String), &b.History)
		}
		items = append(items, &b)
	}
	return items, rows.Err()
}

func (p *PostgresStore) SaveMatchResult(match MatchResult) error {
	if p == nil || p.DB == nil { return nil }
	_, err := p.DB.Exec(`INSERT INTO reconciliation_match_results (id, run_id, evidence_id, internal_id, strategy, matched, break_id, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`, match.ID, match.RunID, match.EvidenceID, match.InternalID, string(match.Strategy), match.Matched, match.BreakID, match.CreatedAt)
	return err
}

func (p *PostgresStore) LoadMatchResults() ([]MatchResult, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT id, run_id, evidence_id, internal_id, strategy, matched, break_id, created_at FROM reconciliation_match_results ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]MatchResult, 0)
	for rows.Next() {
		var m MatchResult
		var strategy string
		if err := rows.Scan(&m.ID, &m.RunID, &m.EvidenceID, &m.InternalID, &strategy, &m.Matched, &m.BreakID, &m.CreatedAt); err != nil { return nil, err }
		m.Strategy = MatchStrategy(strategy)
		items = append(items, m)
	}
	return items, rows.Err()
}

func (p *PostgresStore) SaveWatermark(w Watermark) error {
	if p == nil || p.DB == nil { return nil }
	_, err := p.DB.Exec(`INSERT INTO reconciliation_watermarks (provider, account, currency, watermark, updated_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (provider, account, currency) DO UPDATE SET watermark=EXCLUDED.watermark, updated_at=EXCLUDED.updated_at`, w.Provider, w.Account, w.Currency, w.Value, w.UpdatedAt)
	return err
}

func (p *PostgresStore) LoadWatermarks() ([]Watermark, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT provider, account, currency, watermark, updated_at FROM reconciliation_watermarks ORDER BY updated_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]Watermark, 0)
	for rows.Next() {
		var w Watermark
		if err := rows.Scan(&w.Provider, &w.Account, &w.Currency, &w.Value, &w.UpdatedAt); err != nil { return nil, err }
		items = append(items, w)
	}
	return items, rows.Err()
}

func (p *PostgresStore) SaveApproval(a *Approval) error {
	if p == nil || p.DB == nil || a == nil { return nil }
	payload, _ := json.Marshal(a.ProposedEffect)
	_, err := p.DB.Exec(`INSERT INTO reconciliation_approvals (id, break_id, proposer, approver, reason, evidence_ids, proposed_ledger_effect, resulting_journal_ref, status, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, approver=EXCLUDED.approver, updated_at=EXCLUDED.updated_at`, a.ID, a.BreakID, a.Proposer, a.Approver, a.Reason, pqArray(a.EvidenceIDs), payload, a.ResultingJournalRef, a.Status, a.CreatedAt, a.UpdatedAt)
	return err
}

func (p *PostgresStore) LoadApprovals() ([]*Approval, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT id, break_id, proposer, approver, reason, evidence_ids, proposed_ledger_effect, resulting_journal_ref, status, created_at, updated_at FROM reconciliation_approvals ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]*Approval, 0)
	for rows.Next() {
		var a Approval
		var evidenceIDs sql.NullString
		var payload sql.NullString
		if err := rows.Scan(&a.ID, &a.BreakID, &a.Proposer, &a.Approver, &a.Reason, &evidenceIDs, &payload, &a.ResultingJournalRef, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil { return nil, err }
		a.EvidenceIDs = parseStringList(evidenceIDs)
		json.Unmarshal([]byte(payload.String), &a.ProposedEffect)
		items = append(items, &a)
	}
	return items, rows.Err()
}

func (p *PostgresStore) SaveResolution(res *ResolutionEvent) error {
	if p == nil || p.DB == nil || res == nil { return nil }
	payload, _ := json.Marshal(res.Details)
	_, err := p.DB.Exec(`INSERT INTO reconciliation_resolution_history (id, break_id, action, actor, details, created_at) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`, res.ID, res.BreakID, res.Action, res.Actor, payload, res.CreatedAt)
	return err
}

func (p *PostgresStore) LoadResolutionHistory() ([]*ResolutionEvent, error) {
	if p == nil || p.DB == nil { return nil, nil }
	rows, err := p.DB.Query(`SELECT id, break_id, action, actor, details, created_at FROM reconciliation_resolution_history ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	items := make([]*ResolutionEvent, 0)
	for rows.Next() {
		var r ResolutionEvent
		var details sql.NullString
		if err := rows.Scan(&r.ID, &r.BreakID, &r.Action, &r.Actor, &details, &r.CreatedAt); err != nil { return nil, err }
		json.Unmarshal([]byte(details.String), &r.Details)
		items = append(items, &r)
	}
	return items, rows.Err()
}

func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func pqArray(values []string) string {
	if len(values) == 0 { return "{}" }
	for i := range values { values[i] = strings.ReplaceAll(values[i], "\"", "\"") }
	return "{" + strings.Join(values, ",") + "}"
}

func parseStringList(value sql.NullString) []string {
	if !value.Valid || value.String == "" || value.String == "{}" { return nil }
	trimmed := strings.Trim(value.String, "{}")
	if trimmed == "" { return nil }
	parts := strings.Split(trimmed, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		result = append(result, strings.Trim(p, "\""))
	}
	return result
}
