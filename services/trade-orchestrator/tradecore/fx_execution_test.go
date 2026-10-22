package tradecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFXSubmitAndRecoveryRequireEvidenceVerifier(t *testing.T) {
	store := NewStore()
	executor := &countingFXExecutor{}
	if _, err := store.SubmitFXExecution(context.Background(), "trade_test", executor, nil); err == nil {
		t.Fatal("FX submission without an evidence verifier was accepted")
	}
	if _, err := store.RecoverFXExecution(context.Background(), "trade_test", executor, nil); err == nil {
		t.Fatal("FX recovery without an evidence verifier was accepted")
	}
	if executor.submitCalls != 0 || executor.queryCalls != 0 {
		t.Fatalf("provider was contacted without a verifier: submit=%d query=%d", executor.submitCalls, executor.queryCalls)
	}
}

type countingFXExecutor struct {
	submitCalls int
	queryCalls  int
}

func (executor *countingFXExecutor) SubmitFX(context.Context, FXExecution) error {
	executor.submitCalls++
	return nil
}

func (executor *countingFXExecutor) QueryFX(context.Context, FXExecution) (FXQueryResult, error) {
	executor.queryCalls++
	return FXQueryResult{}, nil
}

func TestFXQueryEvidenceHashMatchesUnpersistedResponseBytes(t *testing.T) {
	payload := []byte(`{"status":"not_executed","reference":"fxr_test"}`)
	hash := sha256.Sum256(payload)
	execution := &FXExecution{
		ExecutorID: "executor_test", RequestReference: "fxr_test", RecoveryGeneration: 7,
	}
	evidence := FXQueryEvidence{
		ExecutorID: execution.ExecutorID, RequestReference: execution.RequestReference,
		RecoveryGeneration: execution.RecoveryGeneration, QueryReference: "query_test",
		PayloadHash: hex.EncodeToString(hash[:]), ObservedAt: time.Now().UTC(), RawPayload: payload,
	}
	if err := validateFXQueryEvidence(execution, evidence); err != nil {
		t.Fatalf("valid response evidence rejected: %v", err)
	}

	evidence.RawPayload = []byte(`{"status":"executed","reference":"fxr_test"}`)
	if err := validateFXQueryEvidence(execution, evidence); err == nil {
		t.Fatal("response bytes that differ from the recorded hash were accepted")
	}

	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(payload)) {
		t.Fatal("raw provider response was persisted with normalized evidence")
	}
}
