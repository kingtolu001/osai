package tradecore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TradeState models the durable workflow state of a trade.
type TradeState string

const (
	StateCreated         TradeState = "CREATED"
	StateQuoted          TradeState = "QUOTED"
	StateAccepted        TradeState = "ACCEPTED"
	StateFundingRequired TradeState = "FUNDING_REQUIRED"
	StateFunded          TradeState = "FUNDED"
	StateFailed          TradeState = "FAILED"
)

// Trade captures the trade lifecycle owned by the trade-orchestrator service.
type Trade struct {
	ID                   string
	InstitutionID        string
	QuoteID              string
	State                TradeState
	Status               string
	Available            int64
	HoldAmount           int64
	BaseAmountMinor      int64
	BaseCurrency         string
	QuoteCurrency        string
	CorrelationID        string
	SettlementID         string
	BeneficiaryID        string
	SettlementProviderID string
	IdempotencyKey       string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Store struct {
	db          *sql.DB
	mu          sync.Mutex
	byID        map[string]*Trade
	byQuoteID   map[string]*Trade
	byOperation map[string]string // quote_id + idempotency_key -> trade_id
}

func NewStore() *Store {
	return &Store{byID: make(map[string]*Trade), byQuoteID: make(map[string]*Trade), byOperation: make(map[string]string)}
}

func (s *Store) CreateTrade(institutionID, quoteID, idempotencyKey, correlationID string, baseAmountMinor int64, baseCurrency, quoteCurrency string, selection ...string) (*Trade, error) {
	beneficiaryID, providerID := "", ""
	if len(selection) > 0 {
		beneficiaryID = selection[0]
	}
	if len(selection) > 1 {
		providerID = selection[1]
	}
	if strings.TrimSpace(institutionID) == "" {
		return nil, errors.New("institution_id required")
	}
	if strings.TrimSpace(quoteID) == "" {
		return nil, errors.New("quote_id required")
	}
	if baseAmountMinor <= 0 {
		return nil, errors.New("base_amount_minor must be positive")
	}
	if strings.TrimSpace(baseCurrency) == "" {
		return nil, errors.New("base_currency required")
	}
	if strings.TrimSpace(quoteCurrency) == "" {
		return nil, errors.New("quote_currency required")
	}
	key := quoteID + "|" + idempotencyKey
	if idempotencyKey != "" {
		s.mu.Lock()
		if tradeID, ok := s.byOperation[key]; ok {
			trade := s.byID[tradeID]
			s.mu.Unlock()
			if trade != nil {
				if trade.BeneficiaryID != beneficiaryID || trade.SettlementProviderID != providerID {
					return nil, errors.New("trade beneficiary replay conflict")
				}
				return trade, nil
			}
		}
		s.mu.Unlock()
	}
	if existing, ok := s.GetByQuoteID(quoteID); ok {
		if existing.InstitutionID != institutionID || existing.BaseAmountMinor != baseAmountMinor || existing.BaseCurrency != baseCurrency || existing.QuoteCurrency != quoteCurrency || existing.BeneficiaryID != beneficiaryID || existing.SettlementProviderID != providerID {
			return nil, errors.New("trade replay conflict")
		}
		return existing, nil
	}
	trade := &Trade{
		ID:                   "trd_" + uuid.NewString(),
		InstitutionID:        institutionID,
		QuoteID:              quoteID,
		State:                StateAccepted,
		Status:               string(StateAccepted),
		Available:            baseAmountMinor,
		HoldAmount:           baseAmountMinor,
		BaseAmountMinor:      baseAmountMinor,
		BaseCurrency:         baseCurrency,
		QuoteCurrency:        quoteCurrency,
		CorrelationID:        correlationID,
		BeneficiaryID:        beneficiaryID,
		SettlementProviderID: providerID,
		IdempotencyKey:       idempotencyKey,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if s.db != nil {
		return s.createDB(trade)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byQuoteID[quoteID]; ok {
		return existing, nil
	}
	s.byID[trade.ID] = trade
	s.byQuoteID[quoteID] = trade
	if key != "|" {
		s.byOperation[key] = trade.ID
	}
	return trade, nil
}

func (s *Store) GetTrade(tradeID string) (*Trade, bool) {
	if s.db != nil {
		return s.getDB("id", tradeID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trade, ok := s.byID[tradeID]
	if !ok {
		return nil, false
	}
	copy := *trade
	return &copy, true
}

func (s *Store) GetByQuoteID(quoteID string) (*Trade, bool) {
	if s.db != nil {
		return s.getDB("quote", quoteID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trade, ok := s.byQuoteID[quoteID]
	if !ok {
		return nil, false
	}
	copy := *trade
	return &copy, true
}

func (s *Store) SetSettlementID(tradeID, settlementID string) error {
	if s.db != nil {
		return s.setSettlementDB(tradeID, settlementID)
	}
	if strings.TrimSpace(tradeID) == "" {
		return errors.New("trade_id required")
	}
	if strings.TrimSpace(settlementID) == "" {
		return errors.New("settlement_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trade, ok := s.byID[tradeID]
	if !ok {
		return fmt.Errorf("trade %s not found", tradeID)
	}
	trade.SettlementID = settlementID
	trade.UpdatedAt = time.Now().UTC()
	return nil
}

func (t *Trade) Accept() error {
	if t == nil {
		return errors.New("trade is nil")
	}
	if t.State != "" && t.State != StateCreated && t.State != StateQuoted {
		return fmt.Errorf("trade cannot accept from state %s", t.State)
	}
	if t.HoldAmount > 0 && t.Available > 0 && t.HoldAmount > t.Available {
		return fmt.Errorf("hold %d exceeds available %d", t.HoldAmount, t.Available)
	}
	t.State = StateAccepted
	t.Status = string(StateAccepted)
	t.UpdatedAt = time.Now().UTC()
	return nil
}

func (t *Trade) RequireFunding() {
	if t != nil && t.Status == string(StateAccepted) {
		t.Status = string(StateFundingRequired)
		t.State = StateFundingRequired
		t.UpdatedAt = time.Now().UTC()
	}
}
