package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type idempotencyEntry struct {
	InstitutionID string
	Endpoint      string
	Key           string
	Hash          string
	Status        string
	ResponseCode  int
	ResponseBody  map[string]any
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type idempotencyStore struct {
	mu    sync.Mutex
	rows  map[string]*idempotencyEntry
	db    *sql.DB
}

func newIdempotencyStore() *idempotencyStore {
	dsn := strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	if dsn == "" {
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	if err := ensureCustomerAPISchema(db); err != nil {
		_ = db.Close()
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	return &idempotencyStore{rows: map[string]*idempotencyEntry{}, db: db}
}

func (s *idempotencyStore) key(instID, endpoint, key string) string { return instID + "|" + endpoint + "|" + key }

func canonicalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func canonicalHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func ensureCustomerAPISchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS customer_api_idempotency (
			institution_id TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			response_code INTEGER NOT NULL DEFAULT 200,
			response_body JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (institution_id, endpoint, idempotency_key)
		);
	`)
	return err
}

func (s *idempotencyStore) CheckAndStore(instID, endpoint, key string, payload any, responseCode int, responseBody map[string]any) (bool, map[string]any, error) {
	if strings.TrimSpace(key) == "" { return false, nil, fmt.Errorf("missing idempotency key") }
	payloadHash := canonicalHash(payload)
	k := s.key(instID, endpoint, key)
	if s.db != nil {
		var existingHash string
		var storedBody []byte
		err := s.db.QueryRow(`SELECT payload_hash, response_body FROM customer_api_idempotency WHERE institution_id = $1 AND endpoint = $2 AND idempotency_key = $3`, instID, endpoint, key).Scan(&existingHash, &storedBody)
		switch {
		case err == nil:
			if existingHash != payloadHash {
				return false, nil, fmt.Errorf("idempotency key reused with different payload")
			}
			var body map[string]any
			if len(storedBody) > 0 && string(storedBody) != "null" {
				if err := json.Unmarshal(storedBody, &body); err != nil {
					body = map[string]any{}
				}
			}
			if body == nil { body = map[string]any{} }
			return true, body, nil
		case err != sql.ErrNoRows:
			return false, nil, err
		}
		_, err = s.db.Exec(`INSERT INTO customer_api_idempotency (institution_id, endpoint, idempotency_key, payload_hash, response_code, response_body, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW()) ON CONFLICT (institution_id, endpoint, idempotency_key) DO NOTHING`, instID, endpoint, key, payloadHash, responseCode, mapToJSON(responseBody))
		if err != nil {
			return false, nil, err
		}
		if responseBody == nil { responseBody = map[string]any{} }
		return true, responseBody, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[k]; ok {
		if row.Hash == payloadHash {
			return true, row.ResponseBody, nil
		}
		return false, nil, fmt.Errorf("idempotency key reused with different payload")
	}
	entry := &idempotencyEntry{InstitutionID: instID, Endpoint: endpoint, Key: key, Hash: payloadHash, Status: "COMPLETED", ResponseCode: responseCode, ResponseBody: responseBody, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.rows[k] = entry
	return true, responseBody, nil
}

func (s *idempotencyStore) SaveResponse(instID, endpoint, key string, payload any, responseBody map[string]any) {
	if responseBody == nil { responseBody = map[string]any{} }
	if s.db != nil {
		payloadHash := canonicalHash(payload)
		_, err := s.db.Exec(`INSERT INTO customer_api_idempotency (institution_id, endpoint, idempotency_key, payload_hash, response_code, response_body, created_at, updated_at) VALUES ($1, $2, $3, $4, 200, $5, NOW(), NOW()) ON CONFLICT (institution_id, endpoint, idempotency_key) DO UPDATE SET payload_hash = EXCLUDED.payload_hash, response_code = EXCLUDED.response_code, response_body = EXCLUDED.response_body, updated_at = NOW()`, instID, endpoint, key, payloadHash, mapToJSON(responseBody))
		if err != nil {
			// fall through to in-memory only if the persistence layer is unavailable
		}
	}
	k := s.key(instID, endpoint, key)
	s.mu.Lock(); defer s.mu.Unlock();
	if row, ok := s.rows[k]; ok {
		row.ResponseBody = responseBody; row.UpdatedAt = time.Now().UTC(); return
	}
	s.rows[k] = &idempotencyEntry{InstitutionID: instID, Endpoint: endpoint, Key: key, Hash: canonicalHash(payload), Status: "COMPLETED", ResponseCode: 200, ResponseBody: responseBody, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}

func mapToJSON(v map[string]any) any {
	if v == nil { return `{} ` }
	b, err := json.Marshal(v)
	if err != nil { return `{} ` }
	return string(b)
}

func sortedMapKeys(m map[string]any) []string { keys := make([]string, 0, len(m)); for k := range m { keys = append(keys, k)}; sort.Strings(keys); return keys }
