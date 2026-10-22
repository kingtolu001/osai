package ledgerstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/osai/osai/services/ledger/ledgercore"
)

const SandboxAccountingVersion = "osai-sandbox-accounting/v1"

// SandboxAccounting has no production or provider routing entry point. All
// accounts and evidence supplied here are explicit fixtures in a local proof DB.
type SandboxAccounting struct{ db *sql.DB }

type SandboxTerms struct {
	TradeID, InstitutionID   string
	PrincipalMinor, FeeMinor int64
	FX                       *ledgercore.FXExecutionEvidence
}

type SandboxAccountingCommand struct {
	ExpectedRevision                                            int64 `json:",omitempty"`
	PolicyVersion, EventID, Event, Actor, CorrelationID, Reason string
	EvidenceHash, Maker, Checker                                string
	EvidencePayload                                             string
	Terms                                                       SandboxTerms
}

func NewSandboxAccounting(ctx context.Context, db *sql.DB, environment string) (*SandboxAccounting, error) {
	if db == nil || environment != "local-sandbox" {
		return nil, errors.New("explicit local sandbox required")
	}
	var database string
	var local bool
	if err := db.QueryRowContext(ctx, `SELECT current_database(), inet_server_addr() IS NULL OR inet_server_addr() <<= '127.0.0.0/8'::inet OR inet_server_addr() = '::1'::inet OR inet_server_addr() <<= '172.16.0.0/12'::inet`).Scan(&database, &local); err != nil {
		return nil, err
	}
	// Docker-supported local PostgreSQL can report its private bridge address.
	// The caller must also select a dedicated proof DB; no existing Osai DB is accepted.
	if !local || !strings.HasPrefix(database, "osai_proof_") {
		return nil, errors.New("dedicated local proof database required")
	}
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS ledger_sandbox_positions (
 trade_id text PRIMARY KEY, terms_hash text NOT NULL, state text NOT NULL, revision bigint NOT NULL DEFAULT 0);
 ALTER TABLE ledger_sandbox_positions ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 0;
 CREATE TABLE IF NOT EXISTS ledger_sandbox_events (
 event_id text PRIMARY KEY, trade_id text NOT NULL REFERENCES ledger_sandbox_positions(trade_id),
 payload_hash text NOT NULL, state text NOT NULL, policy_version text NOT NULL,
 actor text NOT NULL, correlation_id text NOT NULL, reason text NOT NULL,
 evidence_hash text NOT NULL, maker text NOT NULL, checker text NOT NULL,
 command_json text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());`)
	if err != nil {
		return nil, err
	}
	return &SandboxAccounting{db: db}, nil
}

// Apply atomically commits all currency journals, audit evidence and state.
// It accepts fixture observations, not authenticated real-provider evidence.
func (s *SandboxAccounting) Apply(ctx context.Context, c SandboxAccountingCommand) (string, bool, error) {
	if c.PolicyVersion != SandboxAccountingVersion || c.EventID == "" || c.Actor == "" || c.CorrelationID == "" || c.Reason == "" ||
		!strings.HasPrefix(c.Terms.TradeID, "sbx:") || !strings.HasPrefix(c.Terms.InstitutionID, "sbx:") || c.Terms.PrincipalMinor <= 0 || c.Terms.FeeMinor < 0 || c.Terms.FeeMinor > 9223372036854775807-c.Terms.PrincipalMinor {
		return "", false, errors.New("invalid sandbox command or immutable economics")
	}
	hash, err := hex.DecodeString(c.EvidenceHash)
	if err != nil || len(hash) != 32 {
		return "", false, errors.New("fixture evidence SHA-256 required")
	}
	actualHash := sha256.Sum256([]byte(c.EvidencePayload))
	if !json.Valid([]byte(c.EvidencePayload)) || !strings.EqualFold(c.EvidenceHash, hex.EncodeToString(actualHash[:])) {
		return "", false, errors.New("fixture evidence payload hash mismatch")
	}
	if c.Terms.FX != nil {
		e := c.Terms.FX
		for _, reference := range []string{e.Executor, e.QuoteID, e.ExecutionReference, e.SourceEvidenceReference, e.DestinationEvidenceRef} {
			if !strings.HasPrefix(reference, "sbx:") {
				return "", false, errors.New("sandbox fixture FX references required")
			}
		}
		if e.TradeID != c.Terms.TradeID || e.InstitutionID != c.Terms.InstitutionID || e.SourceCurrency != "NGN" || e.DestinationCurrency != "USD" || e.SourceExponent != 2 || e.DestinationExponent != 2 || e.SourcePrincipalMinor != c.Terms.PrincipalMinor || len(e.Fees) != 0 {
			return "", false, errors.New("FX fixture does not match immutable terms")
		}
		if err := ledgercore.ValidateFXExecutionEvidence(*e); err != nil {
			return "", false, err
		}
		exact := new(big.Rat).SetFrac64(e.SourcePrincipalMinor, e.DestinationProceedsMinor)
		if e.RateDirection == ledgercore.DestinationPerSource {
			exact.Inv(exact)
		}
		rate, _ := new(big.Rat).SetString(e.AppliedRate)
		if exact.Cmp(rate) != 0 {
			return "", false, errors.New("sandbox v1 requires exact FX conversion without remainder")
		}
	}
	if c.Event == "release_return" || c.Event == "refund_fee" || c.Event == "retry_payout" {
		if c.Maker == "" || c.Checker == "" || c.Maker == c.Checker || c.Actor != c.Checker {
			return "", false, errors.New("distinct sandbox maker and checker required")
		}
	}
	raw, _ := json.Marshal(c)
	payload := sha256.Sum256(raw)
	termsRaw, _ := json.Marshal(c.Terms)
	termsHash := sha256.Sum256(termsRaw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	// All changes to a trade and all uses of an event ID serialize independently.
	for _, key := range []string{"sandbox-event:" + c.EventID, "sandbox-trade:" + c.Terms.TradeID} {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return "", false, err
		}
	}
	var existing, state string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,state FROM ledger_sandbox_events WHERE event_id=$1`, c.EventID).Scan(&existing, &state)
	if err == nil {
		if existing != hex.EncodeToString(payload[:]) {
			return "", false, errors.New("sandbox event payload conflict")
		}
		return state, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT terms_hash,state,revision FROM ledger_sandbox_positions WHERE trade_id=$1 FOR UPDATE`, c.Terms.TradeID).Scan(&existing, &state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		if c.Event != "fund" {
			return "", false, errors.New("sandbox funding required")
		}
		state = "NEW"
		if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_sandbox_positions(trade_id,terms_hash,state) VALUES($1,$2,'NEW')`, c.Terms.TradeID, hex.EncodeToString(termsHash[:])); err != nil {
			return "", false, err
		}
	} else if err != nil {
		return "", false, err
	} else if existing != hex.EncodeToString(termsHash[:]) {
		return "", false, errors.New("sandbox terms conflict")
	}
	if revision != c.ExpectedRevision {
		return "", false, errors.New("stale sandbox observation revision")
	}
	journals, next, err := sandboxJournals(c, state)
	if err != nil {
		return "", false, err
	}
	for i, journal := range journals {
		if err = insertFundingJournal(ctx, tx, "sbx:"+c.EventID+fmt.Sprint(":", i), "sandbox."+c.Event, &journal); err != nil {
			return "", false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ledger_sandbox_positions SET state=$2,revision=revision+1 WHERE trade_id=$1`, c.Terms.TradeID, next); err != nil {
		return "", false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger_sandbox_events(event_id,trade_id,payload_hash,state,policy_version,actor,correlation_id,reason,evidence_hash,maker,checker,command_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, c.EventID, c.Terms.TradeID, hex.EncodeToString(payload[:]), next, c.PolicyVersion, c.Actor, c.CorrelationID, c.Reason, c.EvidenceHash, c.Maker, c.Checker, string(raw))
	if err != nil {
		return "", false, err
	}
	return next, false, tx.Commit()
}

func sandboxJournals(c SandboxAccountingCommand, state string) ([]ledgercore.Journal, string, error) {
	var journals []ledgercore.Journal
	move := func(currency, debit, credit string, amount int64) {
		if amount == 0 {
			return
		}
		i := len(journals)
		j := ledgercore.NewJournal(fmt.Sprintf("sbx:%s:%d", c.EventID, i), c.Terms.TradeID, currency)
		j.InstitutionID = c.Terms.InstitutionID
		owner := func(role string) string {
			if role == "custody" {
				return "sbx:custodian"
			}
			if role == "fee_revenue" || role == "fee_refunds" {
				return "sbx:osai"
			}
			return c.Terms.InstitutionID
		}
		j.AddEntry("sbx:"+currency+":"+debit, debit, owner(debit), currency, amount, 0, j.ID+":debit")
		j.AddEntry("sbx:"+currency+":"+credit, credit, owner(credit), currency, 0, amount, j.ID+":credit")
		journals = append(journals, *j)
	}
	total := c.Terms.PrincipalMinor + c.Terms.FeeMinor
	next := ""
	switch c.Event {
	case "fund":
		if state == "NEW" {
			move("NGN", "custody", "available", total)
			next = "AVAILABLE"
		}
	case "reserve":
		if state == "AVAILABLE" {
			move("NGN", "available", "held", total)
			next = "HELD"
		}
	case "unknown":
		if state == "HELD" || state == "FX_HELD" {
			next = "UNKNOWN_" + state
		}
	case "non_execution":
		if state == "HELD" || state == "UNKNOWN_HELD" {
			move("NGN", "held", "available", total)
			next = "RELEASED"
		}
	case "fx_complete":
		if (state == "HELD" || state == "UNKNOWN_HELD") && c.Terms.FX != nil {
			move("NGN", "held", "custody", c.Terms.PrincipalMinor)
			move("USD", "custody", "held", c.Terms.FX.DestinationProceedsMinor)
			next = "FX_HELD"
		}
	case "payout_failed":
		if state == "FX_HELD" || state == "UNKNOWN_FX_HELD" {
			move("USD", "held", "restricted", c.Terms.FX.DestinationProceedsMinor)
			next = "FX_PAYOUT_FAILED"
		}
	case "retry_payout":
		if state == "FX_PAYOUT_FAILED" {
			move("USD", "restricted", "held", c.Terms.FX.DestinationProceedsMinor)
			next = "FX_HELD"
		}
	case "payout_complete":
		if c.Terms.FX == nil && (state == "HELD" || state == "UNKNOWN_HELD") {
			move("NGN", "held", "custody", c.Terms.PrincipalMinor)
			next = "COMPLETE"
		}
		if c.Terms.FX != nil && (state == "FX_HELD" || state == "UNKNOWN_FX_HELD") {
			move("USD", "held", "custody", c.Terms.FX.DestinationProceedsMinor)
			next = "COMPLETE"
		}
		if next != "" {
			move("NGN", "held", "fee_revenue", c.Terms.FeeMinor)
		}
	case "return":
		if state == "COMPLETE" {
			if c.Terms.FX == nil {
				move("NGN", "custody", "restricted", c.Terms.PrincipalMinor)
			} else {
				move("USD", "custody", "restricted", c.Terms.FX.DestinationProceedsMinor)
			}
			next = "RETURNED"
		}
	case "release_return":
		if state == "RETURNED" {
			if c.Terms.FX == nil {
				move("NGN", "restricted", "available", c.Terms.PrincipalMinor)
			} else {
				move("USD", "restricted", "available", c.Terms.FX.DestinationProceedsMinor)
			}
			next = "RETURN_RELEASED"
		}
	case "refund_fee":
		if state == "RETURN_RELEASED" && c.Terms.FeeMinor > 0 {
			move("NGN", "fee_refunds", "available", c.Terms.FeeMinor)
			next = "REFUNDED"
		}
	}
	if next == "" {
		return nil, "", fmt.Errorf("sandbox event %s prohibited in state %s", c.Event, state)
	}
	return journals, next, nil
}

// WithSandboxFees enables the explicit fee fixture policy only in a proof DB.
func (p Postgres) WithSandboxFees(ctx context.Context) (Postgres, error) {
	if !strings.HasPrefix(p.CustodyBankID, "local_test_") && !strings.HasPrefix(p.CustodyBankID, "sbx:") {
		return Postgres{}, errors.New("explicit fixture custody owner required")
	}
	if _, err := NewSandboxAccounting(ctx, p.DB, "local-sandbox"); err != nil {
		return Postgres{}, err
	}
	p.sandboxFees = true
	return p, nil
}
