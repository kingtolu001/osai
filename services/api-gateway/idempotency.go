package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var errIdempotencyConflict = errors.New("idempotency key reused with different payload")

func newPersistentIdempotencyStore() (*idempotencyStore, error) {
	dsn := strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	if dsn == "" {
		return nil, errors.New("OSAI_POSTGRES_DSN required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err = ensureCustomerAPISchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &idempotencyStore{rows: map[string]*idempotencyEntry{}, db: db}, nil
}

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
	mu   sync.Mutex
	rows map[string]*idempotencyEntry
	db   *sql.DB
}

func (s *idempotencyStore) key(instID, endpoint, key string) string {
	return instID + "|" + endpoint + "|" + key
}

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
	if strings.TrimSpace(key) == "" {
		return false, nil, fmt.Errorf("missing idempotency key")
	}
	payloadHash := canonicalHash(payload)
	k := s.key(instID, endpoint, key)
	if s.db != nil {
		result, err := s.db.Exec(`INSERT INTO customer_api_idempotency(institution_id,endpoint,idempotency_key,payload_hash,response_body) VALUES($1,$2,$3,$4,'{}') ON CONFLICT DO NOTHING`, instID, endpoint, key, payloadHash)
		if err != nil {
			return false, nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, nil, err
		}
		if affected == 1 {
			return false, nil, nil
		}
		var existingHash string
		var storedBody []byte
		err = s.db.QueryRow(`SELECT payload_hash, response_body FROM customer_api_idempotency WHERE institution_id = $1 AND endpoint = $2 AND idempotency_key = $3`, instID, endpoint, key).Scan(&existingHash, &storedBody)
		switch {
		case err == nil:
			if existingHash != payloadHash {
				return false, nil, errIdempotencyConflict
			}
			var body map[string]any
			if len(storedBody) > 0 && string(storedBody) != "null" {
				if err := json.Unmarshal(storedBody, &body); err != nil {
					body = map[string]any{}
				}
			}
			if body == nil {
				body = map[string]any{}
			}
			return true, body, nil
		case err != sql.ErrNoRows:
			return false, nil, err
		}
		return false, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[k]; ok {
		if row.Hash == payloadHash {
			return true, row.ResponseBody, nil
		}
		return false, nil, errIdempotencyConflict
	}
	entry := &idempotencyEntry{InstitutionID: instID, Endpoint: endpoint, Key: key, Hash: payloadHash, Status: "COMPLETED", ResponseCode: responseCode, ResponseBody: responseBody, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.rows[k] = entry
	return true, responseBody, nil
}

func (s *idempotencyStore) SaveResponse(instID, endpoint, key string, payload any, responseBody map[string]any) error {
	if responseBody == nil {
		responseBody = map[string]any{}
	}
	if s.db != nil {
		payloadHash := canonicalHash(payload)
		result, err := s.db.Exec(`UPDATE customer_api_idempotency SET response_code=200,response_body=$5,updated_at=now() WHERE institution_id=$1 AND endpoint=$2 AND idempotency_key=$3 AND payload_hash=$4`, instID, endpoint, key, payloadHash, mapToJSON(responseBody))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errIdempotencyConflict
		}
		return nil
	}
	k := s.key(instID, endpoint, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[k]; ok {
		row.ResponseBody = responseBody
		row.UpdatedAt = time.Now().UTC()
		return nil
	}
	s.rows[k] = &idempotencyEntry{InstitutionID: instID, Endpoint: endpoint, Key: key, Hash: canonicalHash(payload), Status: "COMPLETED", ResponseCode: 200, ResponseBody: responseBody, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	return nil
}

func mapToJSON(v map[string]any) any {
	if v == nil {
		return `{} `
	}
	b, err := json.Marshal(v)
	if err != nil {
		return `{} `
	}
	return string(b)
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
