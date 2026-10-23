// Package sandboxrecon defines the signed loopback statement fixture protocol.
// Statements are independently sourced test evidence, never production custody.
package sandboxrecon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Movement struct {
	Reference  string `json:"reference"`
	Kind       string `json:"kind"`
	Currency   string `json:"currency"`
	DeltaMinor int64  `json:"delta_minor"`
}

type Snapshot struct {
	InstitutionID string           `json:"institution_id"`
	TradeID       string           `json:"trade_id"`
	Movements     []Movement       `json:"movements"`
	ClosingMinor  map[string]int64 `json:"closing_minor"`
}

type Request struct {
	InstitutionID string `json:"institution_id"`
	TradeID       string `json:"trade_id"`
	Nonce         string `json:"nonce"`
}
type Statement struct {
	Snapshot   Snapshot  `json:"snapshot"`
	Nonce      string    `json:"nonce"`
	ObservedAt time.Time `json:"observed_at"`
}
type Envelope struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}
type Client struct {
	endpoint, key string
	http          *http.Client
}

func MAC(raw []byte, key string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func New(endpoint, key string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(key, "local-") || len(key) < 32 {
		return nil, errors.New("explicit signed loopback statement fixture required")
	}
	return &Client{endpoint: endpoint, key: key, http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("statement redirects prohibited") }}}, nil
}

func (c *Client) Read(ctx context.Context, institution, trade string) (Statement, string, error) {
	var result Statement
	if c == nil || !strings.HasPrefix(institution, "sbx:") || trade == "" {
		return result, "", errors.New("sandbox statement scope required")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return result, "", err
	}
	raw, _ := json.Marshal(Request{institution, trade, hex.EncodeToString(nonce)})
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return result, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Osai-Sandbox-Statement-Signature", MAC(raw, c.key))
	response, err := c.http.Do(req)
	if err != nil {
		return result, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result, "", errors.New("statement fixture unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return result, "", errors.New("statement fixture response exceeds limit")
	}
	var envelope Envelope
	if json.Unmarshal(body, &envelope) != nil || !hmac.Equal([]byte(envelope.Signature), []byte(MAC(envelope.Payload, c.key))) || json.Unmarshal(envelope.Payload, &result) != nil {
		return Statement{}, "", errors.New("statement signature or payload invalid")
	}
	if result.Nonce != hex.EncodeToString(nonce) || result.Snapshot.InstitutionID != institution || result.Snapshot.TradeID != trade || result.ObservedAt.Before(time.Now().Add(-5*time.Minute)) || result.ObservedAt.After(time.Now().Add(30*time.Second)) || len(result.Snapshot.Movements) > 1000 {
		return Statement{}, "", errors.New("statement scope, nonce, age or bound invalid")
	}
	hash := sha256.Sum256(body)
	return result, hex.EncodeToString(hash[:]), nil
}
