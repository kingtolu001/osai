package simcore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
)

func TestNormalizedWebhookRejectsStaleTimestamp(t *testing.T) {
	adapter := NewNormalizedAdapter(FailureProfile{})
	raw := []byte(`{"type":"transfer.success"}`)
	timestamp := time.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339)
	hash := hmac.New(sha256.New, []byte("sandbox-secret"))
	_, _ = hash.Write([]byte(timestamp + "."))
	_, _ = hash.Write(raw)
	_, err := adapter.VerifyWebhook(raw, map[string]string{"X-Osai-Timestamp": timestamp, "X-Osai-Signature": hex.EncodeToString(hash.Sum(nil))})
	if err == nil || err.(*provider.Error).Class != provider.ErrorValidation {
		t.Fatalf("expected normalized validation error, got %v", err)
	}
}

func TestNormalizedWebhookDeduplicatesEventID(t *testing.T) {
	adapter := NewNormalizedAdapter(FailureProfile{})
	raw := []byte(`{"type":"transfer.success","client_ref":"si_1","status":"CONFIRMED","amount_minor":"100","currency":"USD","beneficiary":"acct_1"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	hash := hmac.New(sha256.New, []byte("sandbox-secret"))
	_, _ = hash.Write([]byte(timestamp + "."))
	_, _ = hash.Write(raw)
	headers := map[string]string{"X-Osai-Timestamp": timestamp, "X-Osai-Signature": hex.EncodeToString(hash.Sum(nil)), "X-Provider-Event-Id": "evt_1"}
	first, err := adapter.VerifyWebhook(raw, headers)
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.VerifyWebhook(raw, headers)
	if err != nil || first.ProviderEventID != second.ProviderEventID {
		t.Fatalf("expected replay deduplication, first=%+v second=%+v err=%v", first, second, err)
	}
}
