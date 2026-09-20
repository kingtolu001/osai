package webhookcore

import "testing"

func TestOutboundSignatureAndVerification(t *testing.T) {
	secret := "customer-secret"
	body := []byte(`{"event_type":"trade.accepted"}`)
	timestamp := "2026-09-20T00:00:00Z"
	signed, err := SignOutboundRequest(secret, timestamp, body)
	if err != nil {
		t.Fatalf("sign outbound request: %v", err)
	}
	if signed == "" {
		t.Fatal("signature should not be empty")
	}
	ok, err := VerifyOutboundRequest(secret, timestamp, body, signed)
	if err != nil || !ok {
		t.Fatalf("signature verification should pass: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyOutboundRequest(secret, timestamp, []byte(`{"event_type":"trade.failed"}`), signed); ok {
		t.Fatal("modified body should not verify")
	}
	if _, err := VerifyOutboundRequest(secret, "bad-ts", body, signed); err == nil {
		t.Fatal("invalid timestamp should be rejected")
	}
}
