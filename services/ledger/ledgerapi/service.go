package ledgerapi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/services/ledger/ledgercore"
)

type SettlementInstruction struct {
	ID          string
	ClientRef   string
	Beneficiary string
	AmountMinor int64
	Currency    string
	Purpose     string
}

type Command struct {
	IdempotencyKey    string
	CorrelationID     string
	InstitutionID     string
	SettlementID      string
	TradeID           string
	ObligationID      string
	ProviderReference string
	ExternalReference string
	Beneficiary       string
	AmountMinor       int64
	Currency          string
	Purpose           string
}

type PrefundCommand struct {
	InstitutionID     string
	Currency          string
	AmountMinor       int64
	EvidenceReference string
	CorrelationID     string
}

type FundingCommand struct {
	InstitutionID        string
	TradeID              string
	Currency             string
	AmountMinor          int64
	SourcePrincipalMinor int64
	FeeMinor             int64
	PricingHash          string
	PricingSnapshot      *pricing.Snapshot
	CorrelationID        string
}

type FundingReservation struct {
	TradeID              string
	InstitutionID        string
	Currency             string
	AmountMinor          int64
	SourcePrincipalMinor int64
	FeeMinor             int64
	PricingHash          string
	PricingSnapshot      *pricing.Snapshot
	State                string
	JournalID            string
	SettlementID         string
	SettlementRef        string
}

var ErrInsufficientFunding = errors.New("insufficient institution prefund")
var ErrFundingMismatch = errors.New("funding reservation mismatch")

type FundingStorage interface {
	PostLocalPrefund(context.Context, PrefundCommand) (string, bool, error)
	ReserveFunding(context.Context, FundingCommand) (FundingReservation, bool, error)
	GetFundingReservation(context.Context, FundingCommand) (FundingReservation, error)
	FinalizeFunding(context.Context, Command, bool) (string, bool, error)
}

type ReconciliationAdjustmentCommand struct {
	IdempotencyKey string
	CorrelationID  string
	BreakID        string
	Maker          string
	Checker        string
	Currency       string
	AmountMinor    int64
	Reason         string
	JournalTag     string
}

var ErrIdempotencyConflict = errors.New("idempotency key payload conflict")
var ErrAdjustmentPolicyRequired = errors.New("reconciliation adjustment posting requires approved account targeting, accounting policy and authenticated approval evidence")

// Service is the authoritative ledger-service command boundary. It owns journal
// creation and deduplicates terminal settlement effects by external reference.
type Service struct {
	mu           sync.Mutex
	journals     map[string]ledgercore.Journal
	storage      Storage
	funding      FundingStorage
	correlations []string
}

type Storage interface {
	SaveJournal(string, ledgercore.Journal) error
	LoadJournals() (map[string]ledgercore.Journal, error)
}

func NewService() *Service {
	return &Service{journals: make(map[string]ledgercore.Journal), correlations: make([]string, 0)}
}

func NewServiceWithStorage(storage Storage) (*Service, error) {
	journals, err := storage.LoadJournals()
	if err != nil {
		return nil, err
	}
	service := &Service{journals: journals, storage: storage, correlations: make([]string, 0)}
	service.funding, _ = storage.(FundingStorage)
	return service, nil
}

func (s *Service) PostLocalPrefund(ctx context.Context, command PrefundCommand) (string, bool, error) {
	if s.funding == nil {
		return "", false, errors.New("durable funding storage required")
	}
	return s.funding.PostLocalPrefund(ctx, command)
}

func (s *Service) ReserveFunding(ctx context.Context, command FundingCommand) (FundingReservation, bool, error) {
	if s.funding == nil {
		return FundingReservation{}, false, errors.New("durable funding storage required")
	}
	return s.funding.ReserveFunding(ctx, command)
}

func (s *Service) GetFundingReservation(ctx context.Context, command FundingCommand) (FundingReservation, error) {
	if s.funding == nil {
		return FundingReservation{}, errors.New("durable funding storage required")
	}
	return s.funding.GetFundingReservation(ctx, command)
}

func (s *Service) ConfirmSettlement(instruction SettlementInstruction) error {
	return s.postTerminal(instruction, "settlement.confirmed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", instruction.Beneficiary, instruction.Currency, instruction.AmountMinor, 0, instruction.ClientRef+":confirmed:debit")
		journal.AddEntry("1300", "CLIENT_FUNDS_BANK", "custody", instruction.Currency, 0, instruction.AmountMinor, instruction.ClientRef+":confirmed:credit")
	})
}

func (s *Service) ConfirmCommand(command Command) (string, bool, error) {
	if s.funding != nil {
		return s.funding.FinalizeFunding(context.Background(), command, true)
	}
	return s.postCommand(command, "settlement.confirmed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", command.Beneficiary, command.Currency, command.AmountMinor, 0, command.ExternalReference+":debit")
		journal.AddEntry("1300", "CLIENT_FUNDS_BANK", "custody", command.Currency, 0, command.AmountMinor, command.ExternalReference+":credit")
	})
}

func (s *Service) FailCommand(command Command) (string, bool, error) {
	if s.funding != nil {
		return s.funding.FinalizeFunding(context.Background(), command, false)
	}
	return s.postCommand(command, "settlement.failed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", command.Beneficiary, command.Currency, command.AmountMinor, 0, command.ExternalReference+":debit")
		journal.AddEntry("2000", "CLIENT_PREFUND", command.Beneficiary, command.Currency, 0, command.AmountMinor, command.ExternalReference+":credit")
	})
}

func (s *Service) ReverseCommand(command Command) (string, bool, error) {
	if s.funding != nil {
		return "", false, errors.New("funded reversal requires an approved reversal workflow")
	}
	return s.postCommand(command, "settlement.reversed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", command.Beneficiary, command.Currency, command.AmountMinor, 0, command.ExternalReference+":debit")
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", command.Beneficiary, command.Currency, 0, command.AmountMinor, command.ExternalReference+":credit")
	})
}

func (s *Service) PostReconciliationAdjustment(command ReconciliationAdjustmentCommand) (string, bool, error) {
	if command.IdempotencyKey == "" || command.BreakID == "" || command.AmountMinor <= 0 || command.Currency == "" {
		return "", false, errors.New("invalid reconciliation adjustment command")
	}
	if command.Maker == "" || command.Checker == "" {
		return "", false, errors.New("maker and checker are required")
	}
	if command.Maker == command.Checker {
		return "", false, errors.New("maker and checker must be different actors")
	}
	// Maker/checker names do not establish account ownership or authenticated
	// approval. The current command cannot express an approved economic target.
	// Do not infer a customer or balancing entry from either actor's identity.
	return "", false, ErrAdjustmentPolicyRequired
}

func (s *Service) FailSettlement(instruction SettlementInstruction) error {
	return s.postTerminal(instruction, "settlement.failed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", instruction.Beneficiary, instruction.Currency, instruction.AmountMinor, 0, instruction.ClientRef+":failed:debit")
		journal.AddEntry("2000", "CLIENT_PREFUND", instruction.Beneficiary, instruction.Currency, 0, instruction.AmountMinor, instruction.ClientRef+":failed:credit")
	})
}

func (s *Service) ReverseSettlement(instruction SettlementInstruction) error {
	return s.postTerminal(instruction, "settlement.reversed", func(journal *ledgercore.Journal) {
		journal.AddEntry("2200", "CLIENT_SETTLEMENT_PAYABLE", instruction.Beneficiary, instruction.Currency, instruction.AmountMinor, 0, instruction.ClientRef+":reversed:debit")
		journal.AddEntry("2100", "CLIENT_PREFUND_HELD", instruction.Beneficiary, instruction.Currency, 0, instruction.AmountMinor, instruction.ClientRef+":reversed:credit")
	})
}

func (s *Service) postTerminal(instruction SettlementInstruction, kind string, build func(*ledgercore.Journal)) error {
	if instruction.ClientRef == "" || instruction.AmountMinor <= 0 || instruction.Currency == "" {
		return errors.New("invalid settlement ledger command")
	}
	journal := ledgercore.NewJournal("jt_"+instruction.ClientRef+":"+kind, instruction.ID, instruction.Currency)
	build(journal)
	if err := journal.Validate(); err != nil {
		return fmt.Errorf("validate %s: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.journals[instruction.ClientRef+":"+kind]; exists {
		return nil
	}
	s.journals[instruction.ClientRef+":"+kind] = *journal
	return nil
}

func (s *Service) Correlations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.correlations...)
}

func (s *Service) Journals() []ledgercore.Journal {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ledgercore.Journal, 0, len(s.journals))
	for _, journal := range s.journals {
		result = append(result, journal)
	}
	return result
}

func (s *Service) postCommand(command Command, kind string, build func(*ledgercore.Journal)) (string, bool, error) {
	if command.IdempotencyKey == "" || command.ExternalReference == "" || command.AmountMinor <= 0 || command.Currency == "" {
		return "", false, errors.New("invalid ledger command")
	}
	tradeID := command.TradeID
	if tradeID == "" {
		tradeID = command.SettlementID
	}
	journal := ledgercore.NewJournal("jt_"+command.ExternalReference, tradeID, command.Currency)
	journal.InstitutionID = command.InstitutionID
	build(journal)
	if err := journal.Validate(); err != nil {
		return "", false, fmt.Errorf("validate %s: %w", kind, err)
	}
	key := command.IdempotencyKey + ":" + kind
	s.mu.Lock()
	defer s.mu.Unlock()
	s.correlations = append(s.correlations, command.CorrelationID)
	if existing, ok := s.journals[key]; ok {
		if existing.TradeID != tradeID || existing.InstitutionID != command.InstitutionID || existing.Currency != command.Currency || existing.Entries[0].Debit != command.AmountMinor {
			return "", false, ErrIdempotencyConflict
		}
		return existing.ID, true, nil
	}
	if s.storage != nil {
		if err := s.storage.SaveJournal(key, *journal); err != nil {
			return "", false, err
		}
	}
	s.journals[key] = *journal
	return journal.ID, false, nil
}
