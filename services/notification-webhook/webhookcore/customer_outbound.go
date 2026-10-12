package webhookcore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type CustomerWebhookConfig struct {
	InstitutionID        string    `json:"institution_id"`
	EndpointURL          string    `json:"endpoint_url"`
	Secret               string    `json:"-"`
	Status               string    `json:"status"`
	SubscribedEventTypes []string  `json:"subscribed_event_types,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type OutboundEvent struct {
	EventID       string    `json:"event_id"`
	InstitutionID string    `json:"institution_id"`
	EventType     string    `json:"event_type"`
	EventVersion  string    `json:"event_version"`
	CorrelationID string    `json:"correlation_id"`
	Payload       string    `json:"payload"`
	OccurredAt    time.Time `json:"occurred_at"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
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

func SignCustomerPayload(secret, eventID, timestamp string, body []byte) (string, error) {
	if secret == "" || eventID == "" || timestamp == "" {
		return "", errors.New("secret, event ID, and timestamp are required")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(eventID + "." + timestamp + "."))
	if err != nil {
		return "", err
	}
	_, err = mac.Write(body)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func VerifyCustomerPayload(secret, eventID, timestamp string, body []byte, signature string) (bool, error) {
	parsedTimestamp, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		parsedTimestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return false, errors.New("invalid webhook timestamp")
		}
	}
	delta := time.Since(parsedTimestamp)
	if delta < -5*time.Minute || delta > 5*time.Minute {
		return false, errors.New("webhook timestamp outside allowed tolerance")
	}
	expected, err := SignCustomerPayload(secret, eventID, timestamp, body)
	if err != nil {
		return false, err
	}
	return hmac.Equal([]byte(expected), []byte(signature)), nil
}

func DeliverCustomerEvent(client *http.Client, cfg CustomerWebhookConfig, event OutboundEvent) (*DeliveryAttempt, error) {
	if cfg.InstitutionID == "" || cfg.EndpointURL == "" || cfg.Secret == "" {
		return nil, errors.New("webhook configuration is incomplete")
	}
	if err := validateCustomerWebhookURL(context.Background(), cfg.EndpointURL); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"event_id":       event.EventID,
		"event_type":     event.EventType,
		"event_version":  event.EventVersion,
		"correlation_id": event.CorrelationID,
		"payload":        json.RawMessage(event.Payload),
		"occurred_at":    event.OccurredAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, errors.New("event payload is empty")
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature, err := SignCustomerPayload(cfg.Secret, event.EventID, timestamp, payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.EndpointURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Osai-Timestamp", timestamp)
	req.Header.Set("X-Osai-Event-Id", event.EventID)
	req.Header.Set("X-Osai-Event-Type", event.EventType)
	req.Header.Set("X-Osai-Event-Version", event.EventVersion)
	req.Header.Set("X-Osai-Correlation-Id", event.CorrelationID)
	req.Header.Set("X-Osai-Signature", signature)
	timeout := 10 * time.Second
	if client != nil && client.Timeout > 0 {
		timeout = client.Timeout
	}
	resp, err := newCustomerWebhookHTTPClient(timeout).Do(req)
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
		EventID:       event.EventID,
		InstitutionID: cfg.InstitutionID,
		AttemptNumber: 1,
		AttemptedAt:   time.Now().UTC(),
		HTTPStatus:    status,
		Outcome:       outcome,
		CompletedAt:   time.Now().UTC(),
		NextRetryAt:   time.Now().UTC().Add(5 * time.Minute),
		ErrorMetadata: fmt.Sprintf("http_status=%d", status),
	}
	if status >= 500 || status == 429 {
		attempt.Outcome = "retryable_error"
		attempt.NextRetryAt = time.Now().UTC().Add(1 * time.Minute)
	}
	return &attempt, nil
}
