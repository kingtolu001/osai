package tradecore

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/pricing"
)

// TradeState models the durable workflow state of a trade.
type TradeState string

const (
	StateCreated               TradeState = "CREATED"
	StateQuoted                TradeState = "QUOTED"
	StateAccepted              TradeState = "ACCEPTED"
	StateFundingRequired       TradeState = "FUNDING_REQUIRED"
	StateFunded                TradeState = "FUNDED"
	StateFailed                TradeState = "FAILED"
	StateSettling              TradeState = "SETTLING"
	StateSettled               TradeState = "SETTLED"
	StateFeeAccountingRequired TradeState = "FEE_ACCOUNTING_REQUIRED"
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
	CustomerReference    string
	IdempotencyKey       string
	PricingSnapshot      *pricing.Snapshot
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type TradeListFilter struct {
	Page              int
	PageSize          int
	From              time.Time
	To                time.Time
	Currency          string
	Status            string
	CustomerReference string
}

type Store struct {
	db             *sql.DB
	mu             sync.Mutex
	byID           map[string]*Trade
	byQuoteID      map[string]*Trade
	byOperation    map[string]string // quote_id + idempotency_key -> trade_id
	finalityEvents map[string]FinalityEvent
}

type FinalityEvent struct {
	EventID       string
	InstitutionID string
	SettlementID  string
	TradeID       string
	Status        TradeState
	CorrelationID string
}

func NewStore() *Store {
	return &Store{byID: make(map[string]*Trade), byQuoteID: make(map[string]*Trade), byOperation: make(map[string]string), finalityEvents: make(map[string]FinalityEvent)}
}

func (s *Store) CreateTrade(institutionID, quoteID, idempotencyKey, correlationID string, baseAmountMinor int64, baseCurrency, quoteCurrency string, selection ...string) (*Trade, error) {
	return s.createTrade(institutionID, quoteID, idempotencyKey, correlationID, baseAmountMinor, baseCurrency, quoteCurrency, nil, selection...)
}

func (s *Store) CreateTradeWithPricing(institutionID, quoteID, idempotencyKey, correlationID string, baseAmountMinor int64, baseCurrency, quoteCurrency string, snapshot pricing.Snapshot, selection ...string) (*Trade, error) {
	return s.createTrade(institutionID, quoteID, idempotencyKey, correlationID, baseAmountMinor, baseCurrency, quoteCurrency, &snapshot, selection...)
}

func (s *Store) createTrade(institutionID, quoteID, idempotencyKey, correlationID string, baseAmountMinor int64, baseCurrency, quoteCurrency string, snapshot *pricing.Snapshot, selection ...string) (*Trade, error) {
	beneficiaryID, providerID := "", ""
	if len(selection) > 0 {
		beneficiaryID = selection[0]
	}
	if len(selection) > 1 {
		providerID = selection[1]
	}
	customerReference := ""
	if len(selection) > 2 {
		customerReference = selection[2]
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
				if trade.BeneficiaryID != beneficiaryID || trade.SettlementProviderID != providerID ||
					trade.CustomerReference != customerReference || !samePricingSnapshot(trade.PricingSnapshot, snapshot) {
					return nil, errors.New("trade beneficiary replay conflict")
				}
				return cloneTrade(trade), nil
			}
		}
		s.mu.Unlock()
	}
	if existing, ok := s.GetByQuoteID(quoteID); ok {
		if existing.InstitutionID != institutionID || existing.BaseAmountMinor != baseAmountMinor ||
			existing.BaseCurrency != baseCurrency || existing.QuoteCurrency != quoteCurrency ||
			existing.BeneficiaryID != beneficiaryID || existing.SettlementProviderID != providerID ||
			existing.CustomerReference != customerReference || !samePricingSnapshot(existing.PricingSnapshot, snapshot) {
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
		CustomerReference:    customerReference,
		IdempotencyKey:       idempotencyKey,
		PricingSnapshot:      clonePricingSnapshot(snapshot),
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if s.db != nil {
		return s.createDB(trade)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byQuoteID[quoteID]; ok {
		if existing.InstitutionID != institutionID || existing.BaseAmountMinor != baseAmountMinor ||
			existing.BaseCurrency != baseCurrency || existing.QuoteCurrency != quoteCurrency ||
			existing.BeneficiaryID != beneficiaryID || existing.SettlementProviderID != providerID ||
			existing.CustomerReference != customerReference || !samePricingSnapshot(existing.PricingSnapshot, snapshot) {
			return nil, errors.New("trade replay conflict")
		}
		return cloneTrade(existing), nil
	}
	s.byID[trade.ID] = trade
	s.byQuoteID[quoteID] = trade
	if key != "|" {
		s.byOperation[key] = trade.ID
	}
	return cloneTrade(trade), nil
}

func samePricingSnapshot(left, right *pricing.Snapshot) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func clonePricingSnapshot(snapshot *pricing.Snapshot) *pricing.Snapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	return &copy
}

func cloneTrade(trade *Trade) *Trade {
	if trade == nil {
		return nil
	}
	copy := *trade
	copy.PricingSnapshot = clonePricingSnapshot(trade.PricingSnapshot)
	return &copy
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
	return cloneTrade(trade), true
}

func (s *Store) ListTrades(institutionID string, filter TradeListFilter) ([]Trade, int, error) {
	if strings.TrimSpace(institutionID) == "" {
		return nil, 0, errors.New("institution_id required")
	}
	if filter.Page < 1 || filter.PageSize < 1 || filter.PageSize > 100 {
		return nil, 0, errors.New("invalid pagination")
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && !filter.From.Before(filter.To) {
		return nil, 0, errors.New("from must be earlier than to")
	}
	if s.db != nil {
		return s.listTradesDB(institutionID, filter)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make([]Trade, 0)
	for _, trade := range s.byID {
		if trade.InstitutionID != institutionID ||
			(filter.Status != "" && trade.Status != filter.Status) ||
			(filter.Currency != "" && !strings.EqualFold(trade.BaseCurrency, filter.Currency)) ||
			(filter.CustomerReference != "" && trade.CustomerReference != filter.CustomerReference) ||
			(!filter.From.IsZero() && trade.CreatedAt.Before(filter.From)) ||
			(!filter.To.IsZero() && !trade.CreatedAt.Before(filter.To)) {
			continue
		}
		matches = append(matches, *cloneTrade(trade))
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].ID > matches[j].ID
		}
		return matches[i].CreatedAt.After(matches[j].CreatedAt)
	})
	total := len(matches)
	start := (filter.Page - 1) * filter.PageSize
	if start >= total {
		return []Trade{}, total, nil
	}
	end := start + filter.PageSize
	if end > total {
		end = total
	}
	return matches[start:end], total, nil
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
	return cloneTrade(trade), true
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

func (s *Store) SetStatus(tradeID string, state TradeState) error {
	if tradeID == "" || state == "" {
		return errors.New("trade and state required")
	}
	if s.db != nil {
		return s.setStatusDB(tradeID, state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trade := s.byID[tradeID]
	if trade == nil {
		return errors.New("trade not found")
	}
	trade.State, trade.Status, trade.UpdatedAt = state, string(state), time.Now().UTC()
	return nil
}

func (s *Store) ApplySettlementFinality(event FinalityEvent) error {
	if event.EventID == "" || event.InstitutionID == "" || event.SettlementID == "" || event.TradeID == "" || event.CorrelationID == "" ||
		(event.Status != StateSettled && event.Status != StateFailed) {
		return errors.New("invalid settlement finality event")
	}
	if s.db != nil {
		return s.applySettlementFinalityDB(event)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.finalityEvents[event.EventID]; ok {
		if previous != event {
			return errors.New("settlement finality event collision")
		}
		return nil
	}
	trade := s.byID[event.TradeID]
	if trade == nil {
		return errors.New("trade not found")
	}
	if trade.InstitutionID != event.InstitutionID || trade.SettlementID != event.SettlementID || trade.CorrelationID != event.CorrelationID {
		return errors.New("settlement finality identity mismatch")
	}
	if trade.State != StateSettling && trade.State != event.Status {
		return errors.New("settlement finality conflicts with trade state")
	}
	trade.State, trade.Status, trade.UpdatedAt = event.Status, string(event.Status), time.Now().UTC()
	s.finalityEvents[event.EventID] = event
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
