package webhookcore

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type CustomerWebhookConfig struct {
	InstitutionID       string    `json:"institution_id"`
	EndpointURL         string    `json:"endpoint_url"`
	Secret              string    `json:"-"`
	Status              string    `json:"status"`
	SubscribedEventTypes []string `json:"subscribed_event_types,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type OutboundEvent struct {
	EventID      string    `json:"event_id"`
	InstitutionID string   `json:"institution_id"`
	EventType    string    `json:"event_type"`
	EventVersion string    `json:"event_version"`
	CorrelationID string   `json:"correlation_id"`
	Payload     string    `json:"payload"`
	OccurredAt  time.Time `json:"occurred_at"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type DeliveryAttempt struct {
	DeliveryID    string    `json:"delivery_id"`
	EventID       string    `json:"event_id"`
	InstitutionID string    `json:"institution_id"`
	AttemptNumber int       `json:"attempt_number"`
	AttemptedAt   time.Time `json:"attempted_at"`
	HTTPStatus    int       `json:"http_status"`
	Outcome       string    `json:"outcome"`
	NextRetryAt   time.Time `json:"next_retry_at"`
	CompletedAt   time.Time `json:"completed_at"`
	ErrorMetadata string    `json:"error_metadata,omitempty"`
}

type DeliveryStore struct {
	mu       sync.Mutex
	configs  map[string]*CustomerWebhookConfig
	events   map[string]*OutboundEvent
	attempts map[string][]*DeliveryAttempt
}

func NewDeliveryStore() *DeliveryStore {
	return &DeliveryStore{configs: map[string]*CustomerWebhookConfig{}, events: map[string]*OutboundEvent{}, attempts: map[string][]*DeliveryAttempt{}}
}

func (s *DeliveryStore) RegisterConfig(cfg CustomerWebhookConfig) *CustomerWebhookConfig {
	if cfg.InstitutionID == "" { return nil }
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.Status == "" { cfg.Status = "ACTIVE" }
	if cfg.CreatedAt.IsZero() { cfg.CreatedAt = time.Now().UTC() }
	cfg.UpdatedAt = time.Now().UTC()
	clone := cfg
	s.configs[cfg.InstitutionID] = &clone
	return &clone
}

func (s *DeliveryStore) EnqueueEvent(ev OutboundEvent) *OutboundEvent {
	if ev.EventID == "" { ev.EventID = "evt_" + uuid.NewString() }
	if ev.InstitutionID == "" { return nil }
	if ev.EventVersion == "" { ev.EventVersion = "v1" }
	if ev.Status == "" { ev.Status = "PENDING" }
	if ev.OccurredAt.IsZero() { ev.OccurredAt = time.Now().UTC() }
	if ev.CreatedAt.IsZero() { ev.CreatedAt = time.Now().UTC() }
	s.mu.Lock(); defer s.mu.Unlock()
	clone := ev
	s.events[ev.EventID] = &clone
	return &clone
}

func (s *DeliveryStore) RecordAttempt(attempt DeliveryAttempt) *DeliveryAttempt {
	if attempt.DeliveryID == "" { attempt.DeliveryID = "dlv_" + uuid.NewString() }
	if attempt.EventID == "" { return nil }
	if attempt.AttemptedAt.IsZero() { attempt.AttemptedAt = time.Now().UTC() }
	s.mu.Lock(); defer s.mu.Unlock()
	list := append([]*DeliveryAttempt{}, s.attempts[attempt.EventID]...)
	list = append(list, &attempt)
	s.attempts[attempt.EventID] = list
	return &attempt
}

func (s *DeliveryStore) Event(eventID string) *OutboundEvent {
	s.mu.Lock(); defer s.mu.Unlock()
	if ev, ok := s.events[eventID]; ok { clone := *ev; return &clone }
	return nil
}

func (s *DeliveryStore) Attempts(eventID string) []*DeliveryAttempt {
	s.mu.Lock(); defer s.mu.Unlock()
	items := make([]*DeliveryAttempt, 0, len(s.attempts[eventID]))
	for _, attempt := range s.attempts[eventID] {
		clone := *attempt
		items = append(items, &clone)
	}
	return items
}

func (s *DeliveryStore) RetryBackoff(attempt int) time.Duration {
	if attempt <= 0 { return 0 }
	return time.Duration(attempt) * time.Minute
}

func SignCustomerPayload(secret, timestamp string, body []byte) (string, error) {
	if secret == "" || timestamp == "" {
		return "", errors.New("secret and timestamp are required")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(timestamp + "."))
	if err != nil { return "", err }
	_, err = mac.Write(body)
	if err != nil { return "", err }
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func VerifyCustomerPayload(secret, timestamp string, body []byte, signature string) (bool, error) {
	expected, err := SignCustomerPayload(secret, timestamp, body)
	if err != nil { return false, err }
	return hmac.Equal([]byte(expected), []byte(signature)), nil
}

func DeliverCustomerEvent(client *http.Client, cfg CustomerWebhookConfig, event OutboundEvent) (*DeliveryAttempt, error) {
	if client == nil { client = http.DefaultClient }
	if cfg.InstitutionID == "" || cfg.EndpointURL == "" || cfg.Secret == "" {
		return nil, errors.New("webhook configuration is incomplete")
	}
	payload, err := json.Marshal(map[string]any{
		"event_id":       event.EventID,
		"event_type":     event.EventType,
		"event_version":  event.EventVersion,
		"correlation_id": event.CorrelationID,
		"payload":        json.RawMessage(event.Payload),
		"occurred_at":    event.OccurredAt.UTC().Format(time.RFC3339),
	})
	if err != nil { return nil, err }
	if len(payload) == 0 { return nil, errors.New("event payload is empty") }
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature, err := SignCustomerPayload(cfg.Secret, timestamp, payload)
	if err != nil { return nil, err }
	req, err := http.NewRequest(http.MethodPost, cfg.EndpointURL, bytes.NewReader(payload))
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Osai-Timestamp", timestamp)
	req.Header.Set("X-Osai-Event-Id", event.EventID)
	req.Header.Set("X-Osai-Event-Type", event.EventType)
	req.Header.Set("X-Osai-Event-Version", event.EventVersion)
	req.Header.Set("X-Osai-Correlation-Id", event.CorrelationID)
	req.Header.Set("X-Osai-Signature", signature)
	resp, err := client.Do(req)
	if err != nil {
		attempt := DeliveryAttempt{EventID: event.EventID, InstitutionID: cfg.InstitutionID, AttemptNumber: 1, HTTPStatus: 0, Outcome: "network_error", NextRetryAt: time.Now().UTC().Add(time.Minute), ErrorMetadata: err.Error()}
		return &attempt, err
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	outcome := "success"
	if status < 200 || status >= 300 {
		outcome = "failed"
	}
	attempt := DeliveryAttempt{
		EventID:        event.EventID,
		InstitutionID:  cfg.InstitutionID,
		AttemptNumber:  1,
		AttemptedAt:    time.Now().UTC(),
		HTTPStatus:     status,
		Outcome:        outcome,
		CompletedAt:    time.Now().UTC(),
		NextRetryAt:    time.Now().UTC().Add(5 * time.Minute),
		ErrorMetadata: fmt.Sprintf("http_status=%d", status),
	}
	if status >= 500 || status == 429 {
		attempt.Outcome = "retryable_error"
		attempt.NextRetryAt = time.Now().UTC().Add(1 * time.Minute)
	}
	return &attempt, nil
}
