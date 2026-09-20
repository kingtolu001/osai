package reconciliation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	providerpkg "github.com/osai/osai/pkg/provider"
)

// BreakType is the reconciliation classification for an external truth mismatch.
type BreakType string

const (
	BreakTypeUnmatchedExternal      BreakType = "unmatched_external"
	BreakTypeMissingExternalEvidence BreakType = "missing_external_evidence"
	BreakTypeDuplicateExternal       BreakType = "duplicate_external"
	BreakTypeAmbiguousMatch          BreakType = "ambiguous_match"
	BreakTypeAmountMismatch          BreakType = "amount_mismatch"
	BreakTypeFeeMismatch             BreakType = "fee_mismatch"
	BreakTypeBalanceMismatch         BreakType = "balance_mismatch"
	BreakTypeLateEvidence            BreakType = "late_evidence"
	BreakTypeCurrencyMismatch        BreakType = "currency_mismatch"
)

const (
	BreakStateOpen                = "OPEN"
	BreakStateUnderReview         = "UNDER_REVIEW"
	BreakStateResolutionProposed  = "RESOLUTION_PROPOSED"
	BreakStateResolved            = "RESOLVED"
	BreakStateReopened            = "REOPENED"
)

// DriftType marks a provider statement scenario that intentionally drifts from expected economics.
type DriftType string

const (
	DriftNone     DriftType = "none"
	DriftAmount   DriftType = "amount_drift"
	DriftBalance  DriftType = "balance_drift"
	DriftStatement DriftType = "statement_drift"
)

type Evidence struct {
	ID             string      `json:"id"`
	Source         string      `json:"source"`
	Provider       string      `json:"provider"`
	Account        string      `json:"account"`
	RawPayloadHash string      `json:"raw_payload_hash"`
	IngestedAt     time.Time   `json:"ingested_at"`
	Watermark      string      `json:"watermark"`
	PagingToken    string      `json:"paging_token,omitempty"`
	AmountMinor    int64       `json:"amount_minor,omitempty"`
	Currency       string      `json:"currency,omitempty"`
	ClientRef      string      `json:"client_ref,omitempty"`
	ProviderRef    string      `json:"provider_ref,omitempty"`
	Beneficiary    string      `json:"beneficiary,omitempty"`
	FeeMinor       int64       `json:"fee_minor,omitempty"`
	CorrelationID  string      `json:"correlation_id,omitempty"`
	RawPayload     interface{} `json:"raw_payload,omitempty"`
}

// MatchStrategy indicates how a provider observation was matched to an internal outcome.
type MatchStrategy string

const (
	MatchStrategyExact    MatchStrategy = "exact"
	MatchStrategyComposite MatchStrategy = "composite"
	MatchStrategyManual   MatchStrategy = "manual"
)

// MatchResult records whether an evidence row matched an internal expectation.
type MatchResult struct {
	ID        string        `json:"id"`
	RunID     string        `json:"run_id,omitempty"`
	EvidenceID string       `json:"evidence_id,omitempty"`
	InternalID string       `json:"internal_id,omitempty"`
	Strategy  MatchStrategy `json:"strategy,omitempty"`
	Matched   bool          `json:"matched"`
	BreakID   string        `json:"break_id,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// Watermark captures the most recently processed provider boundary for a scope.
type Watermark struct {
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	Currency  string    `json:"currency"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Approval records a maker-checker decision for a reconciliation adjustment.
type Approval struct {
	ID                 string         `json:"id"`
	BreakID            string         `json:"break_id"`
	Proposer           string         `json:"proposer"`
	Approver           string         `json:"approver"`
	Reason             string         `json:"reason,omitempty"`
	EvidenceIDs        []string       `json:"evidence_ids,omitempty"`
	ProposedEffect     map[string]any `json:"proposed_ledger_effect,omitempty"`
	ResultingJournalRef string        `json:"resulting_journal_ref,omitempty"`
	Status             string         `json:"status,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// ResolutionEvent records a reconciliation break resolution action.
type ResolutionEvent struct {
	ID        string         `json:"id"`
	BreakID   string         `json:"break_id"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor"`
	Details   map[string]any `json:"details,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// BreakTransition records a full lifecycle state change for a break.
type BreakTransition struct {
	ID            string    `json:"id"`
	BreakID       string    `json:"break_id"`
	PreviousState string    `json:"previous_state"`
	NewState      string    `json:"new_state"`
	Actor         string    `json:"actor"`
	Reason        string    `json:"reason"`
	Timestamp     time.Time `json:"timestamp"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

// ReconciliationBreak captures a reconciliation failure or uncertainty and tracks the
// exact evidence and control information required by the architecture.
type ReconciliationBreak struct {
	ID            string            `json:"id"`
	Type          BreakType         `json:"type"`
	Provider      string            `json:"provider"`
	Currency      string            `json:"currency"`
	Expected      int64             `json:"expected"`
	Observed      int64             `json:"observed"`
	Delta         int64             `json:"delta"`
	Value         int64             `json:"value"`
	Age           time.Duration     `json:"age"`
	AgeSeconds    int64             `json:"age_seconds,omitempty"`
	Owner         string            `json:"owner"`
	State         string            `json:"state"`
	Severity      string            `json:"severity,omitempty"`
	Evidence      []Evidence        `json:"evidence"`
	EvidenceIDs   []string          `json:"evidence_ids,omitempty"`
	InternalIDs   []string          `json:"internal_ids,omitempty"`
	CandidateIDs  []string          `json:"candidate_ids,omitempty"`
	Comments      []string          `json:"comments"`
	Resolution    string            `json:"resolution"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	FirstSeen     time.Time         `json:"first_seen,omitempty"`
	LastSeen      time.Time         `json:"last_seen,omitempty"`
	History       []BreakTransition `json:"history,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// ExpectedTransaction describes the internal ledger truth for one movement.
type ExpectedTransaction struct {
	ID          string
	ClientRef   string
	ProviderRef string
	AmountMinor int64
	Currency    string
	Beneficiary string
	FeeMinor    int64
	Account     string
	Watermark   string
}

// StatementRow is an ingested provider statement or transaction record.
type StatementRow struct {
	ID             string
	Provider       string
	Source         string
	ProviderRef    string
	ClientRef      string
	AmountMinor    int64
	Currency       string
	Beneficiary    string
	FeeMinor       int64
	Account        string
	RawHash        string
	Watermark      string
	IngestionTS    time.Time
	Drift          DriftType
	PagingToken    string
}

type Adjustment struct {
	BreakID     string
	Maker       string
	Checker     string
	AmountMinor int64
	Currency    string
	Comment     string
	Approved    bool
	State       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StoreReaderWriter is the persistence boundary used by the reconciliation runtime.
type StoreReaderWriter interface {
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

// Service holds reconciliation state and evidence in memory. It intentionally never writes ledger tables.
type Service struct {
	mu                 sync.Mutex
	observed           map[string]StatementRow
	breaks             map[string]*ReconciliationBreak
	adjustments        map[string]*Adjustment
	duplicateRows      int
	dedupKeyCounts     map[string]int
	scenarios          map[string]bool
	directLedgerWrites int
	sequence           int
	store              StoreReaderWriter
}

func NewService() *Service {
	return &Service{
		observed:       make(map[string]StatementRow),
		breaks:         make(map[string]*ReconciliationBreak),
		adjustments:    make(map[string]*Adjustment),
		dedupKeyCounts: make(map[string]int),
		scenarios:      make(map[string]bool),
	}
}

func NewServiceWithStore(store StoreReaderWriter) *Service {
	svc := NewService()
	svc.store = store
	if store == nil {
		return svc
	}
	if items, err := store.LoadEvidence(); err == nil {
		seen := make(map[string]struct{}, len(items))
		for _, ev := range items {
			key := ev.RawPayloadHash
			if key == "" {
				key = ev.ID
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			svc.observed[key] = StatementRow{
				ID:          ev.ID,
				Provider:    ev.Provider,
				Source:      ev.Source,
				Account:     ev.Account,
				RawHash:     ev.RawPayloadHash,
				Watermark:   ev.Watermark,
				PagingToken: ev.PagingToken,
				AmountMinor: ev.AmountMinor,
				Currency:    ev.Currency,
				ClientRef:   ev.ClientRef,
				ProviderRef: ev.ProviderRef,
				Beneficiary: ev.Beneficiary,
				FeeMinor:    ev.FeeMinor,
				IngestionTS: ev.IngestedAt,
			}
		}
	}
	if approvals, err := store.LoadApprovals(); err == nil {
		for _, approval := range approvals {
			if approval == nil {
				continue
			}
			adj := &Adjustment{
				BreakID:     approval.BreakID,
				Maker:       approval.Proposer,
				Checker:     approval.Approver,
				AmountMinor: approvalAmountMinor(approval.ProposedEffect["amount_minor"]),
				Currency:    approvalCurrency(approval.ProposedEffect["currency"]),
				Comment:     approval.Reason,
				Approved:    approval.Status == "APPROVED",
				State:       approval.Status,
				CreatedAt:   approval.CreatedAt,
				UpdatedAt:   approval.UpdatedAt,
			}
			if adj.State == "" {
				adj.State = "PROPOSED"
			}
			svc.adjustments[approval.BreakID] = adj
		}
	}
	return svc
}

func (s *Service) NewBreak(breakType BreakType, provider, currency string, expected, observed, delta, value int64, age time.Duration, owner, state string, evidence []Evidence, comments []string, resolution string) *ReconciliationBreak {
	id := fmt.Sprintf("break_%d", s.nextSequence())
	now := time.Now().UTC()
	brk := &ReconciliationBreak{
		ID:         id,
		Type:       breakType,
		Provider:   provider,
		Currency:   currency,
		Expected:   expected,
		Observed:   observed,
		Delta:      delta,
		Value:      value,
		Age:        age,
		Owner:      owner,
		State:      state,
		Evidence:   append([]Evidence(nil), evidence...),
		Comments:   append([]string(nil), comments...),
		Resolution: resolution,
		FirstSeen:  now,
		LastSeen:   now,
		CreatedAt:  now,
		History: []BreakTransition{{
			ID:            fmt.Sprintf("break_event_%d", s.nextSequence()),
			BreakID:       id,
			PreviousState: "",
			NewState:      state,
			Actor:         owner,
			Reason:        resolution,
			Timestamp:     now,
		}},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breaks[brk.ID] = brk
	if s.store != nil {
		_ = s.store.SaveBreak(brk)
	}
	return brk
}

func (s *Service) ReconcileTransaction(expected ExpectedTransaction, observed StatementRow) *ReconciliationBreak {
	if expected.ID == "" && observed.ID == "" {
		return nil
	}
	if observed.ID == "" || observed.AmountMinor == 0 && observed.Currency == "" && observed.Beneficiary == "" {
		return s.newBreak(BreakTypeMissingExternalEvidence, expected.ProviderRef, expected.Currency, expected.AmountMinor, 0, expected.AmountMinor, expected.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"missing external evidence"}, "suspense")
	}
	if observed.Watermark != "" && expected.Watermark != "" {
		obsTS, errObs := parseTime(observed.Watermark)
		expTS, errExp := parseTime(expected.Watermark)
		if errObs == nil && errExp == nil && obsTS.Before(expTS) {
			return s.newBreak(BreakTypeLateEvidence, observed.Provider, observed.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, expTS.Sub(obsTS), "ops", BreakStateOpen, nil, []string{"late evidence"}, "manual_review")
		}
	}
	if observed.Drift == DriftAmount || observed.Drift == DriftStatement {
		s.scenarios["amount_drift"] = true
		s.scenarios["statement_drift"] = true
	}
	if observed.Provider != "" && observed.ProviderRef != "" && observed.Provider != observed.ProviderRef && observed.Provider != expected.ProviderRef {
		return s.newBreak(BreakTypeUnmatchedExternal, observed.Provider, observed.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"provider status conflict"}, "manual_review")
	}
	if expected.AmountMinor == observed.AmountMinor && expected.Currency == observed.Currency && expected.Beneficiary == observed.Beneficiary && expected.FeeMinor == observed.FeeMinor {
		if observed.ProviderRef != "" && expected.ProviderRef != "" && observed.ProviderRef != expected.ProviderRef {
			return s.newBreak(BreakTypeUnmatchedExternal, observed.Provider, observed.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"provider reference mismatch"}, "manual_review")
		}
		return nil
	}
	if expected.Currency != observed.Currency {
		return s.newBreak(BreakTypeCurrencyMismatch, observed.Provider, observed.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"currency mismatch"}, "manual_review")
	}
	if expected.AmountMinor != observed.AmountMinor {
		return s.newBreak(BreakTypeAmountMismatch, observed.Provider, observed.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"amount mismatch"}, "manual_review")
	}
	if expected.FeeMinor != observed.FeeMinor {
		return s.newBreak(BreakTypeFeeMismatch, observed.Provider, observed.Currency, expected.FeeMinor, observed.FeeMinor, expected.FeeMinor-observed.FeeMinor, observed.FeeMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"fee mismatch"}, "manual_review")
	}
	if expected.Currency == observed.Currency && expected.Beneficiary == observed.Beneficiary {
		return nil
	}
	return s.newBreak(BreakTypeUnmatchedExternal, observed.Provider, expected.Currency, expected.AmountMinor, observed.AmountMinor, expected.AmountMinor-observed.AmountMinor, observed.AmountMinor, 0, "reconciliation", BreakStateOpen, nil, []string{"no permitted composite fallback"}, "manual_review")
}

func (s *Service) ReconcileBalance(account string, internalNostro, providerClosing, inFlight int64) *ReconciliationBreak {
	expected := internalNostro - inFlight
	if providerClosing == expected {
		return nil
	}
	return s.newBreak(BreakTypeBalanceMismatch, account, "USD", expected, providerClosing, expected-providerClosing, providerClosing, 0, "treasury", BreakStateOpen, nil, []string{"nostro closing balance mismatch"}, "manual_review")
}

func (s *Service) IngestProviderEvidence(ev providerpkg.Evidence, row StatementRow) (StatementRow, error) {
	if row.Provider == "" {
		row.Provider = ev.Provider
	}
	if row.Source == "" {
		row.Source = "provider"
	}
	if row.ID == "" {
		row.ID = ev.ID
	}
	if row.ProviderRef == "" {
		row.ProviderRef = ev.RequestRef
	}
	if row.RawHash == "" {
		row.RawHash = ev.RawPayloadHash
	}
	if row.Account == "" {
		row.Account = ev.Provider
	}
	return s.IngestStatement(row)
}

func (s *Service) IngestBalanceEvidence(provider, account, currency string, balanceMinor int64, observedAt time.Time, watermark string) error {
	if provider == "" {
		provider = "unknown_provider"
	}
	w := Watermark{Provider: provider, Account: account, Currency: currency, Value: watermark, UpdatedAt: observedAt}
	if w.UpdatedAt.IsZero() {
		w.UpdatedAt = time.Now().UTC()
	}
	if s.store != nil {
		if err := s.store.SaveWatermark(w); err != nil {
			return err
		}
	}
	if balanceMinor == 0 {
		return nil
	}
	return nil
}

func (s *Service) IngestStatement(row StatementRow) (StatementRow, error) {
	if row.ID == "" {
		row.ID = fmt.Sprintf("ext_%d", s.nextSequence())
	}
	if row.RawHash == "" {
		row.RawHash = hashRow(row)
	}
	if row.Provider == "" {
		row.Provider = "unknown_provider"
	}
	if row.Source == "" {
		row.Source = "statement"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := row.RawHash
	if key == "" {
		key = row.ProviderRef + "|" + row.ClientRef + "|" + fmt.Sprintf("%d", row.AmountMinor) + "|" + row.Currency + "|" + row.Watermark
	}
	if _, ok := s.observed[key]; ok {
		s.duplicateRows++
		s.dedupKeyCounts[key]++
		return row, nil
	}
	row.IngestionTS = time.Now().UTC()
	s.observed[key] = row
	if s.store != nil {
		ev := Evidence{
			ID:             row.ID,
			Source:         row.Source,
			Provider:       row.Provider,
			Account:        row.Account,
			RawPayloadHash: row.RawHash,
			IngestedAt:     row.IngestionTS,
			Watermark:      row.Watermark,
			PagingToken:    row.PagingToken,
			AmountMinor:    row.AmountMinor,
			Currency:       row.Currency,
			ClientRef:      row.ClientRef,
			ProviderRef:    row.ProviderRef,
			Beneficiary:    row.Beneficiary,
			FeeMinor:       row.FeeMinor,
		}
		if err := s.store.SaveEvidence(ev); err != nil {
			return row, err
		}
	}
	return row, nil
}

func (s *Service) ProposalKey(breakID string) string { return breakID }

func (s *Service) TransitionBreak(breakID, newState, actor, reason, correlationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	brk, ok := s.breaks[breakID]
	if !ok {
		return fmt.Errorf("break %s not found", breakID)
	}
	previous := brk.State
	if previous == "" {
		previous = BreakStateOpen
	}
	if !isValidBreakTransition(previous, newState) {
		return fmt.Errorf("invalid break transition %s -> %s", previous, newState)
	}
	brk.History = append(brk.History, BreakTransition{
		ID:            fmt.Sprintf("transition_%d", s.nextSequence()),
		BreakID:       breakID,
		PreviousState: previous,
		NewState:      newState,
		Actor:         actor,
		Reason:        reason,
		Timestamp:     time.Now().UTC(),
		CorrelationID: correlationID,
	})
	brk.State = newState
	brk.LastSeen = time.Now().UTC()
	brk.CorrelationID = correlationID
	if reason != "" {
		brk.Resolution = reason
	}
	if s.store != nil {
		_ = s.store.SaveBreak(brk)
	}
	return nil
}

func (s *Service) ResolveBreak(breakID, actor, reason, correlationID string) error {
	return s.TransitionBreak(breakID, BreakStateResolved, actor, reason, correlationID)
}

func isValidBreakTransition(previous, next string) bool {
	if previous == "" {
		previous = BreakStateOpen
	}
	switch previous {
	case BreakStateOpen:
		return next == BreakStateUnderReview || next == BreakStateResolutionProposed || next == BreakStateResolved
	case BreakStateUnderReview:
		return next == BreakStateResolutionProposed || next == BreakStateResolved || next == BreakStateReopened
	case BreakStateResolutionProposed:
		return next == BreakStateResolved || next == BreakStateReopened || next == BreakStateUnderReview
	case BreakStateReopened:
		return next == BreakStateUnderReview || next == BreakStateResolutionProposed || next == BreakStateResolved
	case BreakStateResolved:
		return next == BreakStateReopened
	default:
		return false
	}
}

func (s *Service) ProposeAdjustment(breakID, maker, checker string, amountMinor int64, currency, comment string) (*Adjustment, error) {
	if maker == "" || checker == "" {
		return nil, fmt.Errorf("maker and checker are required")
	}
	if maker == checker {
		return nil, fmt.Errorf("maker and checker must be different actors")
	}
	adj := &Adjustment{
		BreakID:     breakID,
		Maker:       maker,
		Checker:     checker,
		AmountMinor: amountMinor,
		Currency:    currency,
		Comment:     comment,
		Approved:    false,
		State:       "PROPOSED",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.adjustments[breakID]; ok && existing.State == "APPROVED" {
		return nil, fmt.Errorf("adjustment %s already approved", breakID)
	}
	s.adjustments[breakID] = adj
	if s.store != nil {
		approval := &Approval{
			ID:                 fmt.Sprintf("approval_%s", breakID),
			BreakID:            breakID,
			Proposer:           maker,
			Approver:           checker,
			Reason:             comment,
			ProposedEffect:     map[string]any{"amount_minor": amountMinor, "currency": currency, "break_id": breakID},
			Status:             "PROPOSED",
			CreatedAt:          adj.CreatedAt,
			UpdatedAt:          adj.UpdatedAt,
		}
		_ = s.store.SaveApproval(approval)
	}
	return adj, nil
}

func (s *Service) ApproveAdjustment(breakID, maker, checker string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	adj, ok := s.adjustments[breakID]
	if !ok {
		return fmt.Errorf("adjustment %s not found", breakID)
	}
	if adj.Maker == checker || adj.Checker == maker {
		return fmt.Errorf("same actor cannot approve own proposal")
	}
	if adj.Maker != maker || adj.Checker != checker {
		return fmt.Errorf("maker/checker mismatch")
	}
	if adj.State == "APPROVED" {
		return fmt.Errorf("adjustment %s already approved", breakID)
	}
	if adj.State == "REJECTED" {
		return fmt.Errorf("adjustment %s rejected and cannot be approved", breakID)
	}
	adj.State = "APPROVED"
	adj.Approved = true
	adj.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		approval := &Approval{
			ID:                 fmt.Sprintf("approval_%s", breakID),
			BreakID:            breakID,
			Proposer:           adj.Maker,
			Approver:           adj.Checker,
			Reason:             adj.Comment,
			ProposedEffect:     map[string]any{"amount_minor": adj.AmountMinor, "currency": adj.Currency, "break_id": breakID},
			Status:             "APPROVED",
			CreatedAt:          adj.CreatedAt,
			UpdatedAt:          adj.UpdatedAt,
		}
		_ = s.store.SaveApproval(approval)
	}
	return nil
}

func (s *Service) RejectAdjustment(breakID, checker, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	adj, ok := s.adjustments[breakID]
	if !ok {
		return fmt.Errorf("adjustment %s not found", breakID)
	}
	if adj.State == "APPROVED" {
		return fmt.Errorf("approved adjustment cannot be rejected")
	}
	if adj.Checker != checker {
		return fmt.Errorf("checker mismatch")
	}
	adj.State = "REJECTED"
	adj.Approved = false
	adj.UpdatedAt = time.Now().UTC()
	if reason != "" {
		adj.Comment = reason
	}
	if s.store != nil {
		approval := &Approval{
			ID:                 fmt.Sprintf("approval_%s", breakID),
			BreakID:            breakID,
			Proposer:           adj.Maker,
			Approver:           adj.Checker,
			Reason:             adj.Comment,
			ProposedEffect:     map[string]any{"amount_minor": adj.AmountMinor, "currency": adj.Currency, "break_id": breakID},
			Status:             "REJECTED",
			CreatedAt:          adj.CreatedAt,
			UpdatedAt:          adj.UpdatedAt,
		}
		_ = s.store.SaveApproval(approval)
	}
	return nil
}

func (s *Service) HasApprovedAdjustment(breakID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	adj, ok := s.adjustments[breakID]
	return ok && adj.Approved
}

func (s *Service) CloneForReplay() *Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := NewService()
	for k, row := range s.observed {
		clone.observed[k] = row
	}
	for id, brk := range s.breaks {
		copyBreak := *brk
		copyBreak.Evidence = append([]Evidence(nil), brk.Evidence...)
		copyBreak.Comments = append([]string(nil), brk.Comments...)
		clone.breaks[id] = &copyBreak
	}
	for id, adj := range s.adjustments {
		copyAdj := *adj
		clone.adjustments[id] = &copyAdj
	}
	return clone
}

func (s *Service) Evidence() []Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Evidence, 0, len(s.observed))
	for _, row := range s.observed {
		items = append(items, Evidence{
			ID:             row.ID,
			Source:         row.Source,
			Provider:       row.Provider,
			Account:        row.Account,
			RawPayloadHash: row.RawHash,
			IngestedAt:     row.IngestionTS,
			Watermark:      row.Watermark,
			PagingToken:    row.PagingToken,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (s *Service) DuplicateRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicateRows
}

func (s *Service) DedupCount() int {
	return s.DuplicateRows()
}

func (s *Service) HasScenario(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.scenarios[name]
	return ok
}

func (s *Service) DirectLedgerWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.directLedgerWrites
}

func (s *Service) nextSequence() int {
	s.sequence++
	return s.sequence
}

func (s *Service) newBreak(breakType BreakType, provider, currency string, expected, observed, delta, value int64, age time.Duration, owner, state string, evidence []Evidence, comments []string, resolution string) *ReconciliationBreak {
	brk := s.NewBreak(breakType, provider, currency, expected, observed, delta, value, age, owner, state, evidence, comments, resolution)
	if provider == "" {
		brk.Provider = "provider_unknown"
	}
	if currency == "" {
		brk.Currency = "USD"
	}
	return brk
}

func approvalAmountMinor(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case string:
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func approvalCurrency(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return "USD"
}

func hashRow(row StatementRow) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{row.ProviderRef, row.ClientRef, fmt.Sprintf("%d", row.AmountMinor), row.Currency, row.Beneficiary, row.Account, row.Watermark}, "|")))
	return hex.EncodeToString(sum[:])
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.Parse(time.RFC3339, value)
}
