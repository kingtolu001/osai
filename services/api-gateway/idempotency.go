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

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var errIdempotencyConflict = errors.New("idempotency key reused with different payload")
var errIdempotencyInProgress = errors.New("idempotency request is still in progress")

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
	LockToken     string
	Status        string
	ResponseCode  int
	ResponseBody  map[string]any
	LockedUntil   time.Time
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

func canonicalHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode idempotency payload: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
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
			status TEXT NOT NULL DEFAULT 'IN_FLIGHT',
			locked_until TIMESTAMPTZ NOT NULL DEFAULT now() + interval '30 seconds',
			lock_token TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (institution_id, endpoint, idempotency_key)
		);
		ALTER TABLE customer_api_idempotency ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'COMPLETED';
		ALTER TABLE customer_api_idempotency ADD COLUMN IF NOT EXISTS locked_until TIMESTAMPTZ;
		ALTER TABLE customer_api_idempotency ADD COLUMN IF NOT EXISTS lock_token TEXT;
		UPDATE customer_api_idempotency SET status='IN_FLIGHT', locked_until=now() - interval '1 second'
		WHERE status='COMPLETED' AND response_body='{}'::jsonb;
	`)
	return err
}

func (s *idempotencyStore) CheckAndStore(instID, endpoint, key string, payload any, responseCode int, responseBody map[string]any) (bool, int, map[string]any, string, error) {
	if strings.TrimSpace(key) == "" {
		return false, 0, nil, "", fmt.Errorf("missing idempotency key")
	}
	payloadHash, err := canonicalHash(payload)
	if err != nil {
		return false, 0, nil, "", err
	}
	lockToken := uuid.NewString()
	k := s.key(instID, endpoint, key)
	if s.db != nil {
		result, err := s.db.Exec(`INSERT INTO customer_api_idempotency(institution_id,endpoint,idempotency_key,payload_hash,response_code,response_body,status,locked_until,lock_token) VALUES($1,$2,$3,$4,$5,'{}','IN_FLIGHT',now()+interval '30 seconds',$6) ON CONFLICT DO NOTHING`, instID, endpoint, key, payloadHash, responseCode, lockToken)
		if err != nil {
			return false, 0, nil, "", err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, 0, nil, "", err
		}
		if affected == 1 {
			return false, responseCode, nil, lockToken, nil
		}
		var existingHash string
		var storedBody []byte
		var state string
		var storedCode int
		var lockedUntil sql.NullTime
		err = s.db.QueryRow(`SELECT payload_hash,status,response_body,response_code,locked_until FROM customer_api_idempotency WHERE institution_id=$1 AND endpoint=$2 AND idempotency_key=$3`, instID, endpoint, key).Scan(&existingHash, &state, &storedBody, &storedCode, &lockedUntil)
		switch {
		case err == nil:
			if existingHash != payloadHash {
				return false, 0, nil, "", errIdempotencyConflict
			}
			if state != "COMPLETED" {
				result, updateErr := s.db.Exec(`UPDATE customer_api_idempotency SET status='IN_FLIGHT',locked_until=now()+interval '30 seconds',lock_token=$5,updated_at=now() WHERE institution_id=$1 AND endpoint=$2 AND idempotency_key=$3 AND payload_hash=$4 AND status<>'COMPLETED' AND (locked_until IS NULL OR locked_until<=now())`, instID, endpoint, key, payloadHash, lockToken)
				if updateErr != nil {
					return false, 0, nil, "", updateErr
				}
				affected, updateErr := result.RowsAffected()
				if updateErr != nil {
					return false, 0, nil, "", updateErr
				}
				if affected == 1 {
					return false, responseCode, nil, lockToken, nil
				}
				return false, 0, nil, "", errIdempotencyInProgress
			}
			var body map[string]any
			if len(storedBody) > 0 && string(storedBody) != "null" {
				if err := json.Unmarshal(storedBody, &body); err != nil {
					return false, 0, nil, "", fmt.Errorf("decode stored idempotency response: %w", err)
				}
			}
			if body == nil {
				body = map[string]any{}
			}
			return true, storedCode, body, "", nil
		case err != sql.ErrNoRows:
			return false, 0, nil, "", err
		}
		return false, 0, nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[k]; ok {
		if row.Hash != payloadHash {
			return false, 0, nil, "", errIdempotencyConflict
		}
		if row.Status == "COMPLETED" {
			return true, row.ResponseCode, row.ResponseBody, "", nil
		}
		if row.LockedUntil.After(time.Now()) {
			return false, 0, nil, "", errIdempotencyInProgress
		}
		row.LockedUntil = time.Now().Add(30 * time.Second)
		row.LockToken = lockToken
		row.UpdatedAt = time.Now().UTC()
		return false, responseCode, nil, lockToken, nil
	}
	now := time.Now().UTC()
	entry := &idempotencyEntry{InstitutionID: instID, Endpoint: endpoint, Key: key, Hash: payloadHash, LockToken: lockToken, Status: "IN_FLIGHT", ResponseCode: responseCode, ResponseBody: responseBody, LockedUntil: now.Add(30 * time.Second), CreatedAt: now, UpdatedAt: now}
	s.rows[k] = entry
	return false, responseCode, nil, lockToken, nil
}

func (s *idempotencyStore) SaveResponse(instID, endpoint, key string, payload any, lockToken string, responseCode int, responseBody map[string]any) error {
	if responseBody == nil {
		responseBody = map[string]any{}
	}
	if lockToken == "" || responseCode < 100 || responseCode > 599 {
		return errors.New("idempotency reservation token and valid response code required")
	}
	payloadHash, err := canonicalHash(payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(responseBody)
	if err != nil {
		return fmt.Errorf("encode idempotency response: %w", err)
	}
	if s.db != nil {
		result, err := s.db.Exec(`UPDATE customer_api_idempotency SET response_code=$6,response_body=$7,status='COMPLETED',locked_until=now(),lock_token=NULL,updated_at=now() WHERE institution_id=$1 AND endpoint=$2 AND idempotency_key=$3 AND payload_hash=$4 AND status='IN_FLIGHT' AND lock_token=$5`, instID, endpoint, key, payloadHash, lockToken, responseCode, string(body))
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
		if row.Hash != payloadHash || row.Status != "IN_FLIGHT" || row.LockToken != lockToken {
			return errIdempotencyConflict
		}
		row.ResponseBody = responseBody
		row.ResponseCode = responseCode
		row.Status = "COMPLETED"
		row.LockToken = ""
		row.LockedUntil = time.Time{}
		row.UpdatedAt = time.Now().UTC()
		return nil
	}
	return errors.New("idempotency reservation not found")
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
