package quotecore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osai/osai/pkg/provider"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrQuoteIdempotencyConflict = errors.New("quote idempotency key reused with different payload")

// QuoteRequest is the normalized inbound request for a quote.
type QuoteRequest = provider.QuoteRequest

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
type ProviderQuote = provider.ProviderQuote

// RouteCandidate records the provider-neutral decision that made a quote
// executable. The selected route is the only eligible candidate in the
// currently approved single-liquidity-provider sandbox corridor.
type RouteCandidate struct {
	LiquidityProviderID  string
	SettlementProviderID string
	RateMinor            int64
	FeeMinor             int64
	AmountOutMinor       int64
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
	RouteCandidates  []RouteCandidate
	Pricing          *QuotePricingSnapshot
}

type QuotePricingSnapshot struct {
	Mode                    QuoteMode
	SourcePrincipalMinor    int64
	TotalDebitMinor         int64
	DestinationTargetMinor  int64
	DestinationSurplusMinor int64
	SourceExponent          uint8
	DestinationExponent     uint8
	RateDirection           string
	RateScale               int64
	FeeSchedule             FeeSchedule
}

// QuoteStore retains quote state and enforces idempotent create/accept semantics.
type QuoteStore struct {
	db             *sql.DB
	mu             sync.Mutex
	quotes         map[string]*ExecutableQuote
	keyIndex       map[string]string // key -> quoteID
	acceptanceKeys map[string]string // quoteID + key -> tradeID
}

func cloneExecutableQuote(quote *ExecutableQuote) *ExecutableQuote {
	if quote == nil {
		return nil
	}
	cloned := *quote
	cloned.Request.ProviderIDs = append([]string(nil), quote.Request.ProviderIDs...)
	cloned.RouteCandidates = append([]RouteCandidate(nil), quote.RouteCandidates...)
	if quote.ProviderEvidence != nil {
		cloned.ProviderEvidence = make(map[string]string, len(quote.ProviderEvidence))
		for key, value := range quote.ProviderEvidence {
			cloned.ProviderEvidence[key] = value
		}
	}
	if quote.Pricing != nil {
		pricing := *quote.Pricing
		cloned.Pricing = &pricing
	}
	if quote.AcceptedAt != nil {
		acceptedAt := *quote.AcceptedAt
		cloned.AcceptedAt = &acceptedAt
	}
	return &cloned
}

func NewQuoteStore() *QuoteStore {
	return &QuoteStore{
		quotes:         make(map[string]*ExecutableQuote),
		keyIndex:       make(map[string]string),
		acceptanceKeys: make(map[string]string),
	}
}

func canonicalQuoteRequestHash(req QuoteRequest) string {
	providerIDs := append([]string(nil), req.ProviderIDs...)
	sort.Strings(providerIDs)
	var canonical bytes.Buffer
	_, _ = fmt.Fprintf(&canonical, "%q|%q|%d|%d|%q|%q|%q|%q|%q|",
		req.CustomerID, req.QuoteMode, req.BaseAmountMinor, req.DestinationAmountMinor,
		req.BaseCurrency, req.QuoteCurrency, req.DestinationRail, req.Urgency, req.CustomerReference)
	for _, providerID := range providerIDs {
		_, _ = fmt.Fprintf(&canonical, "%q|", providerID)
	}
	h := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(h[:])
}

func CanonicalQuoteRequestHash(req QuoteRequest) string {
	return canonicalQuoteRequestHash(req)
}

func quoteIDForRequest(req QuoteRequest, requestHash string) string {
	h := sha256.Sum256([]byte(req.IdempotencyKey + "|" + requestHash))
	return "quo_" + hex.EncodeToString(h[:6])
}

func quoteIdempotencyIndexKey(institutionID, idempotencyKey string) string {
	return institutionID + "\x00" + idempotencyKey
}

func (s *QuoteStore) FindByIdempotency(ctx context.Context, institutionID, idempotencyKey, requestHash string) (*ExecutableQuote, bool, error) {
	if s == nil || strings.TrimSpace(institutionID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, errors.New("institution and idempotency key are required")
	}
	var quote *ExecutableQuote
	if s.db != nil {
		var raw []byte
		err := s.db.QueryRowContext(ctx, `SELECT body FROM quotes WHERE institution_id=$1 AND idempotency_key=$2`, institutionID, idempotencyKey).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		var stored ExecutableQuote
		if err = json.Unmarshal(raw, &stored); err != nil {
			return nil, false, err
		}
		quote = &stored
	} else {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, ok := s.keyIndex[quoteIdempotencyIndexKey(institutionID, idempotencyKey)]
		if !ok {
			return nil, false, nil
		}
		quote = s.quotes[id]
	}
	if quote.RequestHash != requestHash {
		return nil, false, ErrQuoteIdempotencyConflict
	}
	return cloneExecutableQuote(quote), true, nil
}

func (s *QuoteStore) Create(req QuoteRequest, providerID string, rateMinor, feeMinor, amountOutMinor int64, expiresAt time.Time) (*ExecutableQuote, error) {
	return s.create(req, providerID, rateMinor, feeMinor, amountOutMinor, expiresAt, nil)
}

func (s *QuoteStore) CreateFromProvider(req QuoteRequest, quote ProviderQuote) (*ExecutableQuote, error) {
	if quote.ProviderID == "" || quote.BaseCurrency != req.BaseCurrency || quote.QuoteCurrency != req.QuoteCurrency || quote.BaseAmountMinor != req.BaseAmountMinor || quote.AmountOutMinor <= 0 || quote.RateMinor <= 0 || quote.FeeMinor < 0 || !quote.ExpiresAt.After(time.Now()) {
		return nil, errors.New("provider quote is invalid")
	}
	return s.createWithCandidates(req, quote.ProviderID, quote.RateMinor, quote.FeeMinor, quote.AmountOutMinor, quote.ExpiresAt, quote.Evidence, []RouteCandidate{{LiquidityProviderID: quote.ProviderID, SettlementProviderID: req.DestinationRail, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, AmountOutMinor: quote.AmountOutMinor}})
}

func (s *QuoteStore) CreatePricedFromProvider(req QuoteRequest, providerQuote ProviderQuote, policy FeeSchedule, priced PricedAmounts) (*ExecutableQuote, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if policy.InstitutionID != req.CustomerID ||
		policy.SourceCurrency != req.BaseCurrency ||
		policy.DestinationCurrency != req.QuoteCurrency {
		return nil, errors.New("pricing schedule scope does not match quote request")
	}
	expected, err := PriceQuote(policy, QuotePricingInput{
		Mode:                   req.QuoteMode,
		SourceAmountMinor:      req.BaseAmountMinor,
		DestinationAmountMinor: req.DestinationAmountMinor,
		RateScaled:             providerQuote.RateMinor,
	})
	if err != nil || expected != priced {
		return nil, errors.New("priced quote does not match approved schedule")
	}
	if providerQuote.ProviderID == "" ||
		providerQuote.BaseCurrency != req.BaseCurrency ||
		providerQuote.QuoteCurrency != req.QuoteCurrency ||
		providerQuote.BaseAmountMinor != priced.SourcePrincipalMinor ||
		providerQuote.AmountOutMinor != priced.DestinationMinor ||
		providerQuote.RateMinor <= 0 ||
		providerQuote.FeeMinor != 0 ||
		!providerQuote.ExpiresAt.After(time.Now()) {
		return nil, errors.New("firm provider quote does not match priced economics")
	}
	snapshot := &QuotePricingSnapshot{
		Mode:                    req.QuoteMode,
		SourcePrincipalMinor:    priced.SourcePrincipalMinor,
		TotalDebitMinor:         priced.TotalDebitMinor,
		DestinationTargetMinor:  req.DestinationAmountMinor,
		DestinationSurplusMinor: priced.DestinationSurplusMinor,
		SourceExponent:          policy.SourceExponent,
		DestinationExponent:     policy.DestinationExponent,
		RateDirection:           req.QuoteCurrency + "_PER_" + req.BaseCurrency,
		RateScale:               RateScale,
		FeeSchedule:             policy,
	}
	return s.createPriced(req, providerQuote, snapshot)
}

func (s *QuoteStore) createPriced(req QuoteRequest, providerQuote ProviderQuote, snapshot *QuotePricingSnapshot) (*ExecutableQuote, error) {
	if req.CustomerID == "" || req.IdempotencyKey == "" || snapshot == nil {
		return nil, errors.New("institution, idempotency key, and pricing snapshot are required")
	}
	requestHash := canonicalQuoteRequestHash(req)
	quote := &ExecutableQuote{
		ID:               quoteIDForRequest(req, requestHash),
		Request:          req,
		RequestHash:      requestHash,
		ProviderID:       providerQuote.ProviderID,
		Status:           QuoteStatusQuoted,
		RateMinor:        providerQuote.RateMinor,
		FeeMinor:         snapshot.TotalDebitMinor - snapshot.SourcePrincipalMinor,
		AmountOutMinor:   providerQuote.AmountOutMinor,
		ExpiresAt:        providerQuote.ExpiresAt,
		CreatedAt:        time.Now().UTC(),
		SettlementRail:   req.DestinationRail,
		ProviderEvidence: providerQuote.Evidence,
		RouteCandidates: []RouteCandidate{{
			LiquidityProviderID:  providerQuote.ProviderID,
			SettlementProviderID: req.DestinationRail,
			RateMinor:            providerQuote.RateMinor,
			FeeMinor:             providerQuote.FeeMinor,
			AmountOutMinor:       providerQuote.AmountOutMinor,
		}},
		Pricing: snapshot,
	}
	if s.db != nil {
		return s.createDB(quote)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	indexKey := quoteIdempotencyIndexKey(req.CustomerID, req.IdempotencyKey)
	if id, ok := s.keyIndex[indexKey]; ok {
		existing := s.quotes[id]
		if existing.RequestHash != requestHash {
			return nil, ErrQuoteIdempotencyConflict
		}
		return cloneExecutableQuote(existing), nil
	}
	s.keyIndex[quoteIdempotencyIndexKey(req.CustomerID, req.IdempotencyKey)] = quote.ID
	s.quotes[quote.ID] = cloneExecutableQuote(quote)
	return cloneExecutableQuote(quote), nil
}

func (s *QuoteStore) create(req QuoteRequest, providerID string, rateMinor, feeMinor, amountOutMinor int64, expiresAt time.Time, evidence map[string]string) (*ExecutableQuote, error) {
	return s.createWithCandidates(req, providerID, rateMinor, feeMinor, amountOutMinor, expiresAt, evidence, nil)
}

func (s *QuoteStore) createWithCandidates(req QuoteRequest, providerID string, rateMinor, feeMinor, amountOutMinor int64, expiresAt time.Time, evidence map[string]string, candidates []RouteCandidate) (*ExecutableQuote, error) {
	if req.CustomerID == "" {
		return nil, errors.New("customer id required")
	}
	if req.BaseAmountMinor <= 0 {
		return nil, errors.New("base amount must be positive")
	}
	if req.IdempotencyKey != "" && s.db == nil {
		s.mu.Lock()
		if qid, ok := s.keyIndex[quoteIdempotencyIndexKey(req.CustomerID, req.IdempotencyKey)]; ok {
			existing := s.quotes[qid]
			if existing.RequestHash != canonicalQuoteRequestHash(req) {
				s.mu.Unlock()
				return nil, ErrQuoteIdempotencyConflict
			}
			copy := cloneExecutableQuote(existing)
			s.mu.Unlock()
			return copy, nil
		}
		s.mu.Unlock()
	}

	requestHash := canonicalQuoteRequestHash(req)
	quote := &ExecutableQuote{
		ID:               quoteIDForRequest(req, requestHash),
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
		ProviderEvidence: evidence,
		RouteCandidates:  candidates,
	}
	if s.db != nil {
		return s.createDB(quote)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey != "" {
		if qid, ok := s.keyIndex[quoteIdempotencyIndexKey(req.CustomerID, req.IdempotencyKey)]; ok {
			existing := s.quotes[qid]
			if existing.RequestHash != requestHash {
				return nil, ErrQuoteIdempotencyConflict
			}
			return cloneExecutableQuote(existing), nil
		}
		s.keyIndex[quoteIdempotencyIndexKey(req.CustomerID, req.IdempotencyKey)] = quote.ID
	}
	s.quotes[quote.ID] = cloneExecutableQuote(quote)
	return cloneExecutableQuote(quote), nil
}

func (s *QuoteStore) Get(quoteID string) (*ExecutableQuote, bool) {
	if s.db != nil {
		quote, ok := s.getDB(quoteID)
		return cloneExecutableQuote(quote), ok
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.quotes[quoteID]
	if !ok {
		return nil, false
	}
	return cloneExecutableQuote(q), true
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
			return cloneExecutableQuote(quote), nil
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
	return cloneExecutableQuote(quote), nil
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
