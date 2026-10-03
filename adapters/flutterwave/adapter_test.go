package flutterwave

import (
	"errors"
	"testing"

	"github.com/osai/osai/pkg/provider"
)

const transferPayload = `{"event":"transfer.completed","event.type":"Transfer","data":{"id":8416497,"created_at":"2021-04-28T17:01:41.000Z","currency":"NGN","amount":100.25,"status":"SUCCESSFUL","reference":"si_1","meta":{"correlation_id":"corr_1"}}}`

func TestVerifyWebhook(t *testing.T) {
	adapter := New(Config{SecretHash: "local-test-hash"})
	for _, tc := range []struct {
		name    string
		headers map[string]string
		body    string
		wantErr error
	}{
		{"missing hash", nil, transferPayload, nil},
		{"wrong hash", map[string]string{"Verif-Hash": "wrong"}, transferPayload, nil},
		{"malformed JSON", map[string]string{"Verif-Hash": "local-test-hash"}, `{`, provider.ErrMalformedWebhook},
		{"unsupported event", map[string]string{"Verif-Hash": "local-test-hash"}, `{"event":"charge.completed","data":{}}`, provider.ErrWebhookIgnored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := adapter.VerifyWebhook([]byte(tc.body), tc.headers)
			if err == nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("unexpected verification result: %v", err)
			}
		})
	}
	event, err := adapter.VerifyWebhook([]byte(transferPayload), map[string]string{"Verif-Hash": "local-test-hash"})
	if err != nil || event.ProviderEventID != "flutterwave:transfer:8416497:SUCCESSFUL" || event.ProviderRef != "8416497" || event.ClientRef != "si_1" || event.Status != provider.TransferConfirmed || event.AmountMinor != 10025 || event.Currency != "NGN" || event.Evidence.Provider != "flutterwave" || event.Evidence.CorrelationID != "corr_1" || event.OccurredAt.IsZero() || event.Evidence.RawPayloadHash == "" {
		t.Fatalf("unexpected normalized transfer: event=%+v err=%v", event, err)
	}
	if _, err := New(Config{}).VerifyWebhook([]byte(transferPayload), map[string]string{"Verif-Hash": ""}); !errors.Is(err, provider.ErrProviderUnavailable) {
		t.Fatalf("missing configured hash must be unavailable: %v", err)
	}
}

func TestExactAmounts(t *testing.T) {
	for _, tc := range []struct {
		amount, currency string
		want             int64
		valid            bool
	}{
		{"100.25", "NGN", 10025, true},
		{"1", "USD", 100, true},
		{"1.001", "USD", 0, false},
		{"1.5", "JPY", 0, false},
		{"1", "XYZ", 0, false},
	} {
		got, err := minorUnits(tc.amount, tc.currency)
		if (err == nil) != tc.valid || tc.valid && got != tc.want {
			t.Fatalf("%s %s: got %d, %v", tc.amount, tc.currency, got, err)
		}
	}
}
