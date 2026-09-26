package webhookcore

import (
	"github.com/google/uuid"
	"sync"
	"time"
)

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
	if cfg.InstitutionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.Status == "" {
		cfg.Status = "ACTIVE"
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now().UTC()
	}
	cfg.UpdatedAt = time.Now().UTC()
	clone := cfg
	s.configs[cfg.InstitutionID] = &clone
	return &clone
}

func (s *DeliveryStore) EnqueueEvent(ev OutboundEvent) *OutboundEvent {
	if ev.EventID == "" {
		ev.EventID = "evt_" + uuid.NewString()
	}
	if ev.InstitutionID == "" {
		return nil
	}
	if ev.EventVersion == "" {
		ev.EventVersion = "v1"
	}
	if ev.Status == "" {
		ev.Status = "PENDING"
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := ev
	s.events[ev.EventID] = &clone
	return &clone
}

func (s *DeliveryStore) RecordAttempt(attempt DeliveryAttempt) *DeliveryAttempt {
	if attempt.DeliveryID == "" {
		attempt.DeliveryID = "dlv_" + uuid.NewString()
	}
	if attempt.EventID == "" {
		return nil
	}
	if attempt.AttemptedAt.IsZero() {
		attempt.AttemptedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]*DeliveryAttempt{}, s.attempts[attempt.EventID]...)
	list = append(list, &attempt)
	s.attempts[attempt.EventID] = list
	return &attempt
}

func (s *DeliveryStore) Event(eventID string) *OutboundEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev, ok := s.events[eventID]; ok {
		clone := *ev
		return &clone
	}
	return nil
}

func (s *DeliveryStore) Attempts(eventID string) []*DeliveryAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]*DeliveryAttempt, 0, len(s.attempts[eventID]))
	for _, attempt := range s.attempts[eventID] {
		clone := *attempt
		items = append(items, &clone)
	}
	return items
}

func (s *DeliveryStore) RetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(attempt) * time.Minute
}
