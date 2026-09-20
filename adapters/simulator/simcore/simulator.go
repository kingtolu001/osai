package simcore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	quotecore "github.com/osai/osai/services/quote/quotecore"
)

// FailureProfile models deterministic provider faults that the simulator can inject.
type FailureProfile struct {
	DelayMs             int
	TimeoutAfterSuccess bool
	DuplicateCallback   bool
	ReorderedEvents     bool
	GhostSuccess        bool
	PartialFail         bool
	Flaky               bool
	StatementDrift      bool
	AmountDriftMinor    int64
	Seed                int64
}

// Simulator implements a first-class provider adapter for deterministic sandbox testing.
type Simulator struct {
	mu             sync.Mutex
	quotes         map[string]quotecore.ProviderQuote
	transfers      map[string]TransferRecord
	transferRefs   map[string]string
	balances       map[string]int64
	transactions   []ExternalTxn
	providerEvents map[string]WebhookEvent
	profile        FailureProfile
}

type TransferInstruction struct {
	ClientRef   string
	Beneficiary string
	AmountMinor int64
	Currency    string
	Purpose     string
	Metadata    map[string]string
}

type TransferRecord struct {
	ClientRef     string
	ProviderRef   string
	Status        string
	AmountMinor   int64
	Currency      string
	Beneficiary   string
	CreatedAt     time.Time
	CompletedAt   *time.Time
	FailureReason string
}

type TransferAck struct {
	ProviderRef      string
	Status           string
	ExpectedFinality string
	Evidence         map[string]string
}

type TransferStatus struct {
	Status        string
	AmountMinor   int64
	Currency      string
	Beneficiary   string
	CompletedAt   *time.Time
	FailureReason string
	Evidence      map[string]string
}

type Balance struct {
	AccountScope string
	Available    int64
	Currency     string
}

type ExternalTxn struct {
	ID          string
	AmountMinor int64
	Currency    string
	Direction   string
	OccurredAt  time.Time
}

type Health struct {
	Up              bool
	LatencyMs       int
	Capabilities    map[string]bool
	DegradedReasons []string
}

type WebhookEvent struct {
	ProviderEventID string
	Type            string
	Payload         map[string]string
}

func NewSimulator(profile FailureProfile) *Simulator {
	return &Simulator{
		quotes:         make(map[string]quotecore.ProviderQuote),
		transfers:      make(map[string]TransferRecord),
		transferRefs:   make(map[string]string),
		balances:       map[string]int64{"cust_default": 1000000},
		transactions:   make([]ExternalTxn, 0),
		providerEvents: make(map[string]WebhookEvent),
		profile:        profile,
	}
}

func (s *Simulator) GetQuote(req quotecore.QuoteRequest) (quotecore.ProviderQuote, error) {
	if s.profile.DelayMs > 0 {
		time.Sleep(time.Duration(s.profile.DelayMs) * time.Millisecond)
	}
	providerQuote := quotecore.ProviderQuote{
		ID:                      "provq_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		ProviderID:              "sim_lp_1",
		BaseCurrency:            req.BaseCurrency,
		QuoteCurrency:           req.QuoteCurrency,
		BaseAmountMinor:         req.BaseAmountMinor,
		RateMinor:               1000,
		FeeMinor:                500,
		AmountOutMinor:          req.BaseAmountMinor,
		SettlementCompatibility: "NGN_USD",
		ExpiresAt:               time.Now().Add(90 * time.Second),
		Evidence:                map[string]string{"method": "getQuote", "currency": req.QuoteCurrency},
	}
	if s.profile.AmountDriftMinor != 0 {
		providerQuote.AmountOutMinor += s.profile.AmountDriftMinor
	}
	if s.profile.GhostSuccess {
		providerQuote.AmountOutMinor = req.BaseAmountMinor + 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotes[providerQuote.ID] = providerQuote
	return providerQuote, nil
}

func (s *Simulator) AcceptQuote(providerQuoteID string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.quotes[providerQuoteID]; !ok {
		return nil, errors.New("provider quote not found")
	}
	return map[string]string{"providerQuoteID": providerQuoteID, "status": "accepted"}, nil
}

func (s *Simulator) CreateTransfer(in TransferInstruction) (TransferAck, error) {
	if s.profile.Flaky && time.Now().UnixNano()%2 == 0 {
		return TransferAck{}, errors.New("provider flaky")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.transfers[in.ClientRef]; ok {
		return TransferAck{ProviderRef: existing.ProviderRef, Status: existing.Status, ExpectedFinality: "provider_state", Evidence: map[string]string{"client_ref": in.ClientRef, "idempotent": "true"}}, nil
	}

	providerRef := "provtx_" + strings.ReplaceAll(in.ClientRef, "si_", "")
	record := TransferRecord{
		ClientRef:   in.ClientRef,
		ProviderRef: providerRef,
		Status:      "ACCEPTED",
		AmountMinor: in.AmountMinor,
		Currency:    in.Currency,
		Beneficiary: in.Beneficiary,
		CreatedAt:   time.Now().UTC(),
	}
	if s.profile.PartialFail {
		record.Status = "FAILED"
		record.FailureReason = "partial_fail"
	}
	if s.profile.TimeoutAfterSuccess {
		record.Status = "PROCESSING"
	}
	s.transfers[in.ClientRef] = record
	s.transferRefs[providerRef] = in.ClientRef
	return TransferAck{ProviderRef: providerRef, Status: record.Status, ExpectedFinality: "provider_state", Evidence: map[string]string{"client_ref": in.ClientRef}}, nil
}

func (s *Simulator) GetTransfer(clientRef string) (TransferStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.transfers[clientRef]
	if !ok {
		return TransferStatus{}, fmt.Errorf("transfer %s not found", clientRef)
	}
	if s.profile.GhostSuccess && record.AmountMinor > 0 {
		status := TransferStatus{Status: "CONFIRMED", AmountMinor: record.AmountMinor + 1, Currency: record.Currency, Beneficiary: record.Beneficiary, Evidence: map[string]string{"reason": "ghost_success"}}
		return status, nil
	}
	if s.profile.PartialFail {
		status := TransferStatus{Status: "FAILED", AmountMinor: record.AmountMinor, Currency: record.Currency, Beneficiary: record.Beneficiary, FailureReason: "partial_fail", Evidence: map[string]string{"reason": "partial_fail"}}
		return status, nil
	}
	return TransferStatus{Status: record.Status, AmountMinor: record.AmountMinor, Currency: record.Currency, Beneficiary: record.Beneficiary, Evidence: map[string]string{"provider_ref": record.ProviderRef}}, nil
}

func (s *Simulator) GetBalance(accountScope string) ([]Balance, error) {
	return []Balance{{AccountScope: accountScope, Available: s.balances[accountScope], Currency: "USD"}}, nil
}

func (s *Simulator) ListTransactions(window any) ([]ExternalTxn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transactions := append([]ExternalTxn(nil), s.transactions...)
	sort.Slice(transactions, func(i, j int) bool { return transactions[i].OccurredAt.Before(transactions[j].OccurredAt) })
	return transactions, nil
}

func (s *Simulator) VerifyWebhook(raw []byte, headers map[string]string) (WebhookEvent, error) {
	if headers["X-Osai-Signature"] == "" {
		return WebhookEvent{}, errors.New("missing signature")
	}
	key := []byte("sandbox-secret")
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(headers["X-Osai-Timestamp"]))
	_, _ = h.Write([]byte("."))
	_, _ = h.Write(raw)
	sig := hex.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(headers["X-Osai-Signature"])) {
		return WebhookEvent{}, errors.New("invalid signature")
	}
	eventID := headers["X-Provider-Event-Id"]
	if eventID == "" {
		digest := sha256.Sum256(raw)
		eventID = "evt_" + hex.EncodeToString(digest[:])
	}
	payload := map[string]string{"status": "accepted"}
	var parsed map[string]string
	if json.Unmarshal(raw, &parsed) == nil {
		for key, value := range parsed {
			payload[key] = value
		}
	}
	eventType := payload["type"]
	if eventType == "" {
		eventType = "transfer.updated"
	}
	event := WebhookEvent{ProviderEventID: eventID, Type: eventType, Payload: payload}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.providerEvents[event.ProviderEventID]; ok {
		return existing, nil
	}
	s.providerEvents[event.ProviderEventID] = event
	return event, nil
}

func (s *Simulator) Health() Health {
	return Health{Up: true, LatencyMs: 35, Capabilities: map[string]bool{"quotes": true, "fx": true, "payout": true}, DegradedReasons: nil}
}

func (s *Simulator) AddTransaction(txn ExternalTxn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transactions = append(s.transactions, txn)
}

func (s *Simulator) IsHealthy() bool { return s.Health().Up }

// HTTP adapter compatibility.
func (s *Simulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("simulator ok"))
}
