package quotecore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// QuoteRequest is the normalized inbound request for a quote.
type QuoteRequest struct {
	CustomerID      string
	BaseAmountMinor int64
	BaseCurrency    string
	QuoteCurrency   string
	DestinationRail string
	Urgency         string
	ProviderIDs     []string
	IdempotencyKey  string
}

// QuoteStatus models the lifecycle of an executable quote.
type QuoteStatus string

const (
	QuoteStatusOpen     QuoteStatus = "OPEN"
	QuoteStatusQuoted   QuoteStatus = "QUOTED"
	QuoteStatusAccepted QuoteStatus = "ACCEPTED"
	QuoteStatusExpired  QuoteStatus = "EXPIRED"
	QuoteStatusRejected QuoteStatus = "REJECTED"
)

// ProviderQuote is the normalized provider quote returned by a provider adapter.
type ProviderQuote struct {
	ID                      string
	ProviderID              string
	BaseCurrency            string
	QuoteCurrency           string
	BaseAmountMinor         int64
	RateMinor               int64
	FeeMinor                int64
	AmountOutMinor          int64
	SettlementCompatibility string
	ExpiresAt               time.Time
	Evidence                map[string]string
}

// ExecutableQuote contains the accepted quote economics for a trade.
type ExecutableQuote struct {
	ID               string
	Request          QuoteRequest
	RequestHash      string
	ProviderID       string
	Status           QuoteStatus
	RateMinor        int64
	FeeMinor         int64
	AmountOutMinor   int64
	ExpiresAt        time.Time
	AcceptedTradeID  string
	AcceptanceKey    string
	AcceptedAt       *time.Time
	CreatedAt        time.Time
	SettlementRail   string
	ProviderEvidence map[string]string
}

// QuoteStore retains quote state and enforces idempotent create/accept semantics.
type QuoteStore struct {
	db             *sql.DB
	mu             sync.Mutex
	quotes         map[string]*ExecutableQuote
	keyIndex       map[string]string // key -> quoteID
	acceptanceKeys map[string]string // quoteID + key -> tradeID
}

func NewQuoteStore() *QuoteStore {
	return &QuoteStore{
		quotes:         make(map[string]*ExecutableQuote),
		keyIndex:       make(map[string]string),
		acceptanceKeys: make(map[string]string),
	}
}

func canonicalQuoteRequestHash(req QuoteRequest) string {
	parts := []string{req.CustomerID, req.BaseCurrency, req.QuoteCurrency, req.DestinationRail, req.Urgency, req.IdempotencyKey}
	parts = append(parts, fmt.Sprintf("%d", req.BaseAmountMinor))
	for _, p := range req.ProviderIDs {
		parts = append(parts, p)
	}
	sort.Strings(req.ProviderIDs)
	parts = append(parts, fmt.Sprintf("%d", req.BaseAmountMinor))
	for _, p := range req.ProviderIDs {
		parts = append(parts, p)
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

func (s *QuoteStore) Create(req QuoteRequest, providerID string, rateMinor, feeMinor, amountOutMinor int64, expiresAt time.Time) (*ExecutableQuote, error) {
	if req.CustomerID == "" {
		return nil, errors.New("customer id required")
	}
	if req.BaseAmountMinor <= 0 {
		return nil, errors.New("base amount must be positive")
	}
	if req.IdempotencyKey != "" && s.db == nil {
		s.mu.Lock()
		if qid, ok := s.keyIndex[req.IdempotencyKey]; ok {
			s.mu.Unlock()
			return s.quotes[qid], nil
		}
		s.mu.Unlock()
	}

	requestHash := canonicalQuoteRequestHash(req)
	quote := &ExecutableQuote{
		ID:               "quo_" + requestHash[:12],
		Request:          req,
		RequestHash:      requestHash,
		ProviderID:       providerID,
		Status:           QuoteStatusQuoted,
		RateMinor:        rateMinor,
		FeeMinor:         feeMinor,
		AmountOutMinor:   amountOutMinor,
		ExpiresAt:        expiresAt,
		CreatedAt:        time.Now().UTC(),
		SettlementRail:   req.DestinationRail,
		ProviderEvidence: map[string]string{},
	}
	if s.db != nil {
		return s.createDB(quote)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey != "" {
		if qid, ok := s.keyIndex[req.IdempotencyKey]; ok {
			return s.quotes[qid], nil
		}
		s.keyIndex[req.IdempotencyKey] = quote.ID
	}
	s.quotes[quote.ID] = quote
	return quote, nil
}

func (s *QuoteStore) Get(quoteID string) (*ExecutableQuote, bool) {
	if s.db != nil {
		return s.getDB(quoteID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.quotes[quoteID]
	if !ok {
		return nil, false
	}
	copy := *q
	return &copy, true
}

func (s *QuoteStore) Accept(quoteID, idempotencyKey, tradeID string, now time.Time) (*ExecutableQuote, error) {
	if s.db != nil {
		return s.acceptDB(quoteID, idempotencyKey, tradeID, "", now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	quote, ok := s.quotes[quoteID]
	if !ok {
		return nil, errors.New("quote not found")
	}
	if quote.Status == QuoteStatusAccepted {
		if quote.AcceptedTradeID == tradeID || idempotencyKey == quote.AcceptanceKey {
			return quote, nil
		}
		return nil, errors.New("quote already accepted for different trade")
	}
	if quote.Status == QuoteStatusExpired || quote.Status == QuoteStatusRejected {
		return nil, fmt.Errorf("quote cannot accept from status %s", quote.Status)
	}
	if now.After(quote.ExpiresAt) || now.Equal(quote.ExpiresAt) {
		quote.Status = QuoteStatusExpired
		return nil, fmt.Errorf("quote expired at %s", quote.ExpiresAt.Format(time.RFC3339))
	}
	quote.Status = QuoteStatusAccepted
	quote.AcceptedTradeID = tradeID
	quote.AcceptanceKey = idempotencyKey
	next := now.UTC()
	quote.AcceptedAt = &next
	return quote, nil
}

func (q *ExecutableQuote) Accept(tradeID, acceptanceKey string, now time.Time, store *QuoteStore) error {
	if store == nil {
		return errors.New("quote store is required")
	}
	updated, err := store.Accept(q.ID, acceptanceKey, tradeID, now)
	if err != nil {
		return err
	}
	*q = *updated
	return nil
}

func (q *ExecutableQuote) IsExpiredAt(now time.Time) bool {
	return !q.ExpiresAt.After(now)
}
