package settlementcore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/osai/osai/pkg/ids"
	"github.com/osai/osai/pkg/provider"
)

type State string

const (
	Created          State = "CREATED"
	Submitted        State = "SUBMITTED"
	ProviderAccepted State = "PROVIDER_ACCEPTED"
	Processing       State = "PROCESSING"
	Confirmed        State = "CONFIRMED"
	Failed           State = "FAILED"
	Unknown          State = "UNKNOWN"
	ManualReview     State = "MANUAL_REVIEW"
)

func (s State) Terminal() bool { return s == Confirmed || s == Failed }

type Instruction struct {
	ID            string
	InstitutionID string
	TradeID       string
	QuoteID       string
	CorrelationID string
	ClientRef     string
	ProviderRef   string
	ProviderID    string
	BankCode      string
	AccountNumber string
	Beneficiary   string
	AmountMinor   int64
	Currency      string
	Purpose       string
	State         State
	Supersedes    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Evidence      []provider.Evidence
}

type Transition struct {
	From       State
	To         State
	Event      string
	Reason     string
	Source     string
	OccurredAt time.Time
}

type LedgerCommand interface {
	ConfirmSettlement(Instruction) error
	FailSettlement(Instruction) error
}

type Persistence interface {
	SaveInstruction(Instruction) error
	SaveTransition(string, Transition) error
	SaveEvidence(string, provider.Evidence) error
	LoadInstructions() ([]Instruction, error)
}

type Store struct {
	mu            sync.Mutex
	instructions  map[string]*Instruction
	transitions   map[string][]Transition
	ledger        LedgerCommand
	effects       map[string]State
	webhookEvents map[string]struct{}
	persistence   Persistence
}

func NewStore(ledger LedgerCommand) *Store {
	return &Store{instructions: make(map[string]*Instruction), transitions: make(map[string][]Transition), ledger: ledger, effects: make(map[string]State), webhookEvents: make(map[string]struct{})}
}

func NewStoreWithPersistence(ledger LedgerCommand, persistence Persistence) (*Store, error) {
	store := NewStore(ledger)
	store.persistence = persistence
	instructions, err := persistence.LoadInstructions()
	if err != nil {
		return nil, err
	}
	for index := range instructions {
		instruction := instructions[index]
		store.instructions[instruction.ID] = &instruction
	}
	return store, nil
}

func (s *Store) Create(beneficiary, currency string, amountMinor int64, purpose string) (Instruction, error) {
	return s.CreateWithInstitution("", beneficiary, currency, amountMinor, purpose)
}

func (s *Store) CreateWithInstitution(institutionID, beneficiary, currency string, amountMinor int64, purpose string) (Instruction, error) {
	if beneficiary == "" || currency == "" || amountMinor <= 0 {
		return Instruction{}, errors.New("beneficiary, currency and positive amount are required")
	}
	now := time.Now().UTC()
	instruction := &Instruction{ID: ids.NewSettlementID(), InstitutionID: institutionID, ClientRef: ids.NewSettlementID(), Beneficiary: beneficiary, AmountMinor: amountMinor, Currency: currency, Purpose: purpose, State: Created, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock()
	s.instructions[instruction.ID] = instruction
	s.mu.Unlock()
	if s.persistence != nil {
		if err := s.persistence.SaveInstruction(*instruction); err != nil {
			return Instruction{}, err
		}
	}
	return *instruction, nil
}

func (s *Store) Get(id string) (Instruction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instruction, ok := s.instructions[id]
	if !ok {
		return Instruction{}, false
	}
	return *instruction, true
}

func (s *Store) Import(instruction Instruction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := instruction
	s.instructions[instruction.ID] = &copy
}

func (s *Store) Transitions(id string) []Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Transition(nil), s.transitions[id]...)
}

func (s *Store) Submit(id string, rail provider.SettlementRail) (Instruction, error) {
	if rail == nil {
		return Instruction{}, errors.New("settlement rail is required")
	}
	instruction, err := s.transition(id, []State{Created}, Submitted, "submit", "workflow", "provider create submitted")
	if err != nil {
		return Instruction{}, err
	}
	ack, callErr := rail.CreateTransfer(provider.TransferInstruction{ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency, Purpose: instruction.Purpose})
	if callErr != nil {
		if ambiguous(callErr) {
			_, _ = s.transition(id, []State{Submitted}, Unknown, "create_ambiguous", "provider", callErr.Error())
			return s.Poll(id, rail)
		}
		return s.transition(id, []State{Submitted}, Failed, "create_rejected", "provider", callErr.Error())
	}
	s.recordEvidence(id, ack.Evidence)
	s.mu.Lock()
	if stored, ok := s.instructions[id]; ok {
		stored.ProviderRef = ack.ProviderRef
	}
	s.mu.Unlock()
	next := ProviderAccepted
	if ack.Status == provider.TransferProcessing {
		next = Processing
	}
	return s.transition(id, []State{Submitted, Unknown}, next, "provider_accepted", "provider", "provider acknowledged transfer")
}

func (s *Store) Poll(id string, rail provider.SettlementRail) (Instruction, error) {
	if rail == nil {
		return Instruction{}, errors.New("settlement rail is required")
	}
	instruction, ok := s.Get(id)
	if !ok {
		return Instruction{}, errors.New("settlement instruction not found")
	}
	if instruction.State.Terminal() {
		return instruction, nil
	}
	result, err := rail.GetTransfer(instruction.ClientRef)
	if err != nil {
		return s.transition(id, []State{Submitted, ProviderAccepted, Processing, Unknown}, Unknown, "poll_ambiguous", "provider", err.Error())
	}
	s.recordEvidence(id, result.Evidence)
	return s.applyResult(id, result)
}

func (s *Store) ResolveWebhook(id string, event provider.WebhookEvent) (Instruction, error) {
	instruction, ok := s.Get(id)
	if !ok {
		return Instruction{}, errors.New("settlement instruction not found")
	}
	if event.ProviderEventID != "" {
		s.mu.Lock()
		if _, seen := s.webhookEvents[event.ProviderEventID]; seen {
			s.mu.Unlock()
			return instruction, nil
		}
		s.webhookEvents[event.ProviderEventID] = struct{}{}
		s.mu.Unlock()
	}
	if event.ClientRef != "" && event.ClientRef != instruction.ClientRef {
		return s.transition(id, []State{Unknown, Submitted, ProviderAccepted, Processing}, ManualReview, "reference_mismatch", "provider", "webhook client reference mismatch")
	}
	s.recordEvidence(id, event.Evidence)
	return s.applyResult(id, provider.TransferResult{Status: event.Status, AmountMinor: event.AmountMinor, Currency: event.Currency, Beneficiary: event.Beneficiary, Evidence: event.Evidence})
}

func (s *Store) applyResult(id string, result provider.TransferResult) (Instruction, error) {
	instruction, ok := s.Get(id)
	if !ok {
		return Instruction{}, errors.New("settlement instruction not found")
	}
	if instruction.State.Terminal() {
		return instruction, nil
	}
	if result.Status == provider.TransferConfirmed {
		if result.AmountMinor != instruction.AmountMinor || result.Currency != instruction.Currency || (result.Beneficiary != "" && result.Beneficiary != instruction.Beneficiary) {
			return s.transition(id, []State{Submitted, ProviderAccepted, Processing, Unknown}, ManualReview, "evidence_mismatch", "provider", "confirmed evidence does not match instruction")
		}
		return s.finish(id, Confirmed, "confirmed", "provider", "finality evidence")
	}
	if result.Status == provider.TransferFailed {
		return s.finish(id, Failed, "authoritative_failure", "provider", result.FailureReason)
	}
	if result.Status == provider.TransferProcessing {
		return s.transition(id, []State{Submitted, ProviderAccepted, Processing, Unknown}, Processing, "processing", "provider", "provider reports processing")
	}
	return s.transition(id, []State{Submitted, ProviderAccepted, Processing, Unknown}, ProviderAccepted, "accepted", "provider", "provider reports acceptance")
}

func (s *Store) finish(id string, to State, event, source, reason string) (Instruction, error) {
	instruction, err := s.transition(id, []State{Submitted, ProviderAccepted, Processing, Unknown}, to, event, source, reason)
	if err != nil {
		return Instruction{}, err
	}
	s.mu.Lock()
	first := s.effects[id] == ""
	if first {
		s.effects[id] = to
	}
	ledger := s.ledger
	s.mu.Unlock()
	if first && ledger != nil {
		if to == Confirmed {
			err = ledger.ConfirmSettlement(instruction)
		} else {
			err = ledger.FailSettlement(instruction)
		}
	}
	return instruction, err
}

func (s *Store) transition(id string, allowed []State, to State, event, source, reason string) (Instruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instruction, ok := s.instructions[id]
	if !ok {
		return Instruction{}, errors.New("settlement instruction not found")
	}
	if instruction.State.Terminal() {
		return *instruction, nil
	}
	valid := false
	for _, from := range allowed {
		if instruction.State == from {
			valid = true
			break
		}
	}
	if !valid {
		return Instruction{}, fmt.Errorf("invalid settlement transition %s -> %s", instruction.State, to)
	}
	now := time.Now().UTC()
	s.transitions[id] = append(s.transitions[id], Transition{From: instruction.State, To: to, Event: event, Source: source, Reason: reason, OccurredAt: now})
	transition := s.transitions[id][len(s.transitions[id])-1]
	instruction.State, instruction.UpdatedAt = to, now
	if s.persistence != nil {
		if err := s.persistence.SaveInstruction(*instruction); err != nil {
			return Instruction{}, err
		}
		if err := s.persistence.SaveTransition(id, transition); err != nil {
			return Instruction{}, err
		}
	}
	return *instruction, nil
}

func (s *Store) recordEvidence(id string, evidence provider.Evidence) {
	if evidence.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if instruction, ok := s.instructions[id]; ok {
		instruction.Evidence = append(instruction.Evidence, evidence)
	}
	if s.persistence != nil {
		_ = s.persistence.SaveEvidence(id, evidence)
	}
}

func ambiguous(err error) bool {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		return providerErr.Class == provider.ErrorProviderTimeout || providerErr.Class == provider.ErrorProviderUnknown || providerErr.Class == provider.ErrorInternal
	}
	return true
}

func (s *Store) EscalateUnknown(id, reason string) (Instruction, error) {
	return s.transition(id, []State{Unknown}, ManualReview, "unknown_age_limit", "system", reason)
}

func (s *Store) CreateSuperseding(id, beneficiary, currency string, amountMinor int64, purpose string) (Instruction, error) {
	old, ok := s.Get(id)
	if !ok {
		return Instruction{}, errors.New("settlement instruction not found")
	}
	if old.State != Failed && old.State != ManualReview {
		return Instruction{}, fmt.Errorf("settlement instruction cannot be superseded from %s", old.State)
	}
	next, err := s.Create(beneficiary, currency, amountMinor, purpose)
	if err != nil {
		return Instruction{}, err
	}
	s.mu.Lock()
	s.instructions[next.ID].Supersedes = old.ID
	next.Supersedes = old.ID
	s.mu.Unlock()
	return next, nil
}
