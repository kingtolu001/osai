// Package sandboxpayout defines an authenticated loopback fixture protocol.
// It is not a Flutterwave API and cannot dispatch a real-provider transfer.
package sandboxpayout

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
	"github.com/osai/osai/pkg/sandboxaccounting"
)

type Instruction struct {
	InstitutionID, TradeID string
	Binding                sandboxaccounting.PayoutBinding
}

type Request struct {
	Operation, Nonce string
	Instruction      Instruction
}

type Payload struct {
	// RETURNED/REVERSED attest a full return of Binding.AmountMinor in the
	// destination currency to fixture custody. They never attest an FX unwind.
	Instruction                               Instruction
	QueryReference, Status, ProviderReference string
	ObservedAt                                time.Time
}

type Envelope struct {
	Payload   json.RawMessage
	Signature string
}

type Observation struct {
	Payload
	Raw []byte
}

type Client struct {
	base string
	key  []byte
	http *http.Client
}

func New(base, key string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || (u.Path != "" && u.Path != "/sandbox-payout") || !strings.HasPrefix(key, "local-") || len(key) < 32 {
		return nil, errors.New("explicit loopback sandbox payout endpoint and fixture signing key required")
	}
	return &Client{base: strings.TrimSuffix(base, "/"), key: []byte(key), http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validInstruction(i Instruction) bool {
	b := i.Binding
	return strings.HasPrefix(i.InstitutionID, "sbx:") && i.TradeID != "" && b.SettlementID != "" && b.ClientReference != "" && b.ProviderID == "sbx:provider" && sandboxaccounting.ValidBeneficiaryID(b.BeneficiaryID) && b.Currency == "USD" && b.AmountMinor > 0
}

func mac(key, raw []byte) []byte { h := hmac.New(sha256.New, key); h.Write(raw); return h.Sum(nil) }

func (c *Client) call(ctx context.Context, operation string, i Instruction) ([]byte, string, error) {
	if c == nil || !validInstruction(i) {
		return nil, "", errors.New("valid sandbox payout instruction required")
	}
	nonce := uuid.NewString()
	raw, err := json.Marshal(Request{Operation: operation, Nonce: nonce, Instruction: i})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+operation, bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Osai-Sandbox-Payout-Signature", hex.EncodeToString(mac(c.key, raw)))
	response, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("sandbox payout outcome unresolved")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || response.StatusCode != http.StatusOK {
		return nil, "", errors.New("sandbox payout outcome unresolved")
	}
	return body, nonce, nil
}

// Submit acknowledgements never establish finality. Callers must durably fence
// the first dispatch before this call and query after every ambiguous outcome.
func (c *Client) Submit(ctx context.Context, i Instruction) error {
	_, _, err := c.call(ctx, "submit", i)
	return err
}

func strictJSON(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("one sandbox payout JSON value required")
	}
	return nil
}

func (c *Client) Query(ctx context.Context, i Instruction) (Observation, error) {
	raw, nonce, err := c.call(ctx, "query", i)
	if err != nil {
		return Observation{}, err
	}
	return c.verify(i, raw, nonce, true)
}

// VerifyStored reauthenticates durable evidence after restart. It deliberately
// preserves the original signed observation time instead of inventing freshness.
func (c *Client) VerifyStored(i Instruction, raw []byte) (Observation, error) {
	return c.verify(i, raw, "", false)
}

func (c *Client) verify(i Instruction, raw []byte, nonce string, fresh bool) (Observation, error) {
	if len(raw) > 1<<20 {
		return Observation{}, errors.New("sandbox payout evidence too large")
	}
	var envelope Envelope
	if strictJSON(raw, &envelope) != nil {
		return Observation{}, errors.New("invalid sandbox payout envelope")
	}
	signature, err := hex.DecodeString(envelope.Signature)
	if err != nil || !hmac.Equal(signature, mac(c.key, envelope.Payload)) {
		return Observation{}, errors.New("invalid sandbox payout signature")
	}
	var p Payload
	if strictJSON(envelope.Payload, &p) != nil || !reflect.DeepEqual(p.Instruction, i) || p.QueryReference == "" || (fresh && p.QueryReference != nonce) || p.ObservedAt.IsZero() || (fresh && time.Since(p.ObservedAt) > 5*time.Minute) || p.ObservedAt.After(time.Now().Add(30*time.Second)) {
		return Observation{}, errors.New("sandbox payout evidence identity or freshness mismatch")
	}
	switch p.Status {
	case "UNKNOWN", "NOT_FOUND", "PENDING":
	case "SUCCEEDED", "FAILED", "RETURNED", "REVERSED":
		if !strings.HasPrefix(p.ProviderReference, "sbx:") {
			return Observation{}, errors.New("fixture payout outcome reference required")
		}
	default:
		return Observation{}, errors.New("unsupported sandbox payout outcome")
	}
	return Observation{Payload: p, Raw: raw}, nil
}
