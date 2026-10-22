package tradecore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
)

// SandboxFXRequest is a simulator contract, not a real-provider API.
type SandboxFXRequest struct {
	Operation string      `json:"operation"`
	Nonce     string      `json:"nonce"`
	Execution FXExecution `json:"execution"`
}

type SandboxFXPayload struct {
	Status              FXQueryStatus                   `json:"status"`
	ExecutorID          string                          `json:"executor_id"`
	RequestReference    string                          `json:"request_reference"`
	RequestHash         string                          `json:"request_hash"`
	PricingSnapshotHash string                          `json:"pricing_snapshot_hash"`
	RecoveryGeneration  int64                           `json:"recovery_generation"`
	ProviderRef         string                          `json:"provider_reference"`
	QueryReference      string                          `json:"query_reference"`
	ObservedAt          time.Time                       `json:"observed_at"`
	Execution           *fxevidence.FXExecutionEvidence `json:"execution,omitempty"`
}

type SandboxFXEnvelope struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

type SandboxFXClient struct {
	base, executorID string
	key              []byte
	http             *http.Client
}

func NewSandboxFXClient(base, executorID, key string) (*SandboxFXClient, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || (u.Path != "" && u.Path != "/sandbox-fx") || !strings.HasPrefix(executorID, "sbx:") || !strings.HasPrefix(key, "local-") || len(key) < 32 {
		return nil, errors.New("explicit loopback sandbox FX endpoint, fixture executor and signing key required")
	}
	return &SandboxFXClient{base: base, executorID: executorID, key: []byte(key), http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func sandboxFXMAC(key, payload []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *SandboxFXClient) call(ctx context.Context, operation string, e FXExecution) ([]byte, string, error) {
	if e.ExecutorID != c.executorID || e.TradeID == "" || e.RequestReference == "" || e.RequestHash == "" || e.PricingSnapshotHash == "" {
		return nil, "", errors.New("sandbox executor identity mismatch")
	}
	nonce := uuid.NewString()
	raw, err := json.Marshal(SandboxFXRequest{Operation: operation, Nonce: nonce, Execution: e})
	if err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+operation, bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Osai-Sandbox-FX-Signature", sandboxFXMAC(c.key, raw))
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", errors.New("sandbox FX response unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, "", errors.New("sandbox FX response incomplete or too large")
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", errors.New("sandbox FX outcome unresolved")
	}
	return body, nonce, nil
}

func (c *SandboxFXClient) SubmitFX(ctx context.Context, e FXExecution) error {
	if e.Status != FXExecutionSubmitted {
		return errors.New("durable SUBMITTED state required before sandbox create")
	}
	// The acknowledgement is not finality. Query authentication is mandatory.
	_, _, err := c.call(ctx, "submit", e)
	return err
}

func (c *SandboxFXClient) decode(e FXExecution, raw []byte) (SandboxFXPayload, error) {
	var envelope SandboxFXEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Payload) == 0 {
		return SandboxFXPayload{}, errors.New("invalid sandbox FX envelope")
	}
	signature, err := hex.DecodeString(envelope.Signature)
	if err != nil {
		return SandboxFXPayload{}, errors.New("invalid sandbox FX signature")
	}
	expected, _ := hex.DecodeString(sandboxFXMAC(c.key, envelope.Payload))
	if !hmac.Equal(signature, expected) {
		return SandboxFXPayload{}, errors.New("sandbox FX authentication failed")
	}
	var payload SandboxFXPayload
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, errors.New("invalid signed sandbox FX payload")
	}
	now := time.Now().UTC()
	if payload.ExecutorID != c.executorID || payload.ExecutorID != e.ExecutorID || payload.RequestReference != e.RequestReference || payload.RequestHash != e.RequestHash || payload.PricingSnapshotHash != e.PricingSnapshotHash || payload.RecoveryGeneration != e.RecoveryGeneration || payload.QueryReference == "" || payload.ObservedAt.Before(now.Add(-5*time.Minute)) || payload.ObservedAt.After(now.Add(30*time.Second)) {
		return payload, errors.New("sandbox FX observation is stale or unbound")
	}
	switch payload.Status {
	case FXQueryPending, FXQueryNotExecuted, FXQueryExecuted, FXQueryFailed, FXQueryUnknown:
	default:
		return payload, errors.New("unknown sandbox FX status")
	}
	if payload.Status == FXQueryExecuted {
		if payload.Execution == nil || !strings.HasPrefix(payload.ProviderRef, "sbx:") || payload.ProviderRef != payload.Execution.ExecutionReference {
			return payload, errors.New("sandbox FX execution reference mismatch")
		}
		if err := validateFXExecution(&e, *payload.Execution); err != nil {
			return payload, err
		}
		if payload.Execution.DestinationProceedsMinor != e.QuotedDestinationMinor {
			return payload, errors.New("sandbox FX proceeds differ from the accepted quote; reconciliation required")
		}
	} else if payload.Execution != nil {
		return payload, errors.New("non-executed response contains execution economics")
	}
	return payload, nil
}

func (c *SandboxFXClient) QueryFX(ctx context.Context, e FXExecution) (FXQueryResult, error) {
	raw, nonce, err := c.call(ctx, "query", e)
	if err != nil {
		return FXQueryResult{}, err
	}
	payload, err := c.decode(e, raw)
	if err != nil {
		return FXQueryResult{}, err
	}
	if payload.QueryReference != nonce {
		return FXQueryResult{}, errors.New("sandbox FX response nonce mismatch")
	}
	digest := sha256.Sum256(raw)
	return FXQueryResult{Status: payload.Status, Execution: payload.Execution, QueryEvidence: FXQueryEvidence{ExecutorID: payload.ExecutorID, RequestReference: payload.RequestReference, RecoveryGeneration: payload.RecoveryGeneration, ProviderRef: payload.ProviderRef, QueryReference: payload.QueryReference, ObservedAt: payload.ObservedAt, PayloadHash: hex.EncodeToString(digest[:]), RawPayload: raw}}, nil
}

func (c *SandboxFXClient) VerifyFXQuery(_ context.Context, e FXExecution, result FXQueryResult) error {
	payload, err := c.decode(e, result.QueryEvidence.RawPayload)
	if err != nil {
		return err
	}
	if result.Status != payload.Status || result.QueryEvidence.ProviderRef != payload.ProviderRef || result.QueryEvidence.QueryReference != payload.QueryReference || !result.QueryEvidence.ObservedAt.Equal(payload.ObservedAt) || !reflect.DeepEqual(result.Execution, payload.Execution) {
		return errors.New("parsed sandbox FX economics do not match authenticated bytes")
	}
	return validateFXQueryEvidence(&e, result.QueryEvidence)
}

// RecoverSandboxFXBatch queries only unresolved instructions belonging to this
// explicit fixture executor. It never submits or automatically spends funds.
// The returned count is verified observations applied, not financial completions.
func (s *Store) RecoverSandboxFXBatch(ctx context.Context, client *SandboxFXClient, limit int) (int, error) {
	if s == nil || s.db == nil || client == nil || client.http == nil || limit < 1 || limit > 100 {
		return 0, errors.New("sandbox recovery configuration incomplete")
	}
	if err := s.ValidateSandboxFXRecovery(ctx); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT trade_id FROM trade_fx_executions WHERE status IN ('SUBMITTED','UNKNOWN') AND body->>'executor_id'=$1 ORDER BY updated_at,execution_id LIMIT $2`, client.executorID, limit)
	if err != nil {
		return 0, err
	}
	var trades []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		trades = append(trades, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	processed := 0
	var failures []error
	for _, tradeID := range trades {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if _, err := s.RecoverFXExecution(ctx, tradeID, client, client); err != nil {
			failures = append(failures, err)
		} else {
			processed++
		}
	}
	return processed, errors.Join(failures...)
}

func (s *Store) ValidateSandboxFXRecovery(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("durable sandbox recovery store required")
	}
	var database string
	if err := s.db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	if !strings.HasPrefix(database, "osai_proof_") {
		return errors.New("sandbox recovery requires proof database")
	}
	return nil
}
