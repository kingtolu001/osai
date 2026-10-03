package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

// Operator identities are provisioned outside the customer API. A token is bound
// to one actor, one role and an explicit institution allowlist.
type beneficiaryOperator struct {
	ActorID      string   `json:"actor_id"`
	Token        string   `json:"token"`
	Role         string   `json:"role"`
	Institutions []string `json:"institutions"`
	tokenHash    [32]byte
}

type beneficiaryOperatorRequest struct {
	InstitutionID string `json:"institution_id"`
	BeneficiaryID string `json:"beneficiary_id"`
	Reason        string `json:"reason"`
}

func loadBeneficiaryOperators() ([]beneficiaryOperator, error) {
	raw := os.Getenv("OSAI_BENEFICIARY_OPERATORS_JSON")
	if raw == "" {
		return nil, nil
	}
	var operators []beneficiaryOperator
	if err := json.Unmarshal([]byte(raw), &operators); err != nil {
		return nil, errors.New("invalid beneficiary operator configuration")
	}
	seenActors := make(map[string]bool)
	seenTokens := make(map[[32]byte]bool)
	for i := range operators {
		op := &operators[i]
		if strings.TrimSpace(op.ActorID) == "" || len(op.Token) < 32 || (op.Role != "maker" && op.Role != "checker") || len(op.Institutions) == 0 || seenActors[op.ActorID] {
			return nil, errors.New("invalid beneficiary operator configuration")
		}
		op.tokenHash = sha256.Sum256([]byte(op.Token))
		if seenTokens[op.tokenHash] {
			return nil, errors.New("duplicate beneficiary operator token")
		}
		seenActors[op.ActorID], seenTokens[op.tokenHash] = true, true
		op.Token = ""
	}
	return operators, nil
}

func (s *Service) beneficiaryOperatorHandler(operators []beneficiaryOperator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		role, action := "", ""
		switch r.URL.Path {
		case "/v1/operator/beneficiaries/propose":
			role, action = "maker", "PROPOSE"
		case "/v1/operator/beneficiaries/approve":
			role, action = "checker", "APPROVE"
		case "/v1/operator/beneficiaries/reject":
			role, action = "checker", "REJECT"
		case "/v1/operator/beneficiaries/disable":
			role, action = "checker", "DISABLE"
		default:
			http.NotFound(w, r)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" || bearer == r.Header.Get("Authorization") {
			http.Error(w, "operator unauthorized", http.StatusUnauthorized)
			return
		}
		candidate := sha256.Sum256([]byte(bearer))
		var actor *beneficiaryOperator
		for i := range operators {
			if subtle.ConstantTimeCompare(candidate[:], operators[i].tokenHash[:]) == 1 && operators[i].Role == role {
				actor = &operators[i]
			}
		}
		if actor == nil {
			http.Error(w, "operator unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var input beneficiaryOperatorRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.InstitutionID) == "" || strings.TrimSpace(input.BeneficiaryID) == "" || strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 500 {
			http.Error(w, "invalid approval request", http.StatusBadRequest)
			return
		}
		allowed := false
		for _, institution := range actor.Institutions {
			allowed = allowed || institution == input.InstitutionID
		}
		if !allowed {
			http.Error(w, "institution forbidden", http.StatusForbidden)
			return
		}
		if err := s.recordBeneficiaryAction(input, action, actor.ActorID); err != nil {
			http.Error(w, "beneficiary action unavailable", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *Service) recordBeneficiaryAction(input beneficiaryOperatorRequest, action, actorID string) error {
	if s.db == nil {
		return errors.New("durable approval store required")
	}
	if err := s.ValidateInstitutionAccess(input.InstitutionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow(`SELECT body FROM customer_beneficiaries WHERE id=$1 AND institution_id=$2 FOR UPDATE`, input.BeneficiaryID, input.InstitutionID).Scan(&raw); err != nil {
		return err
	}
	var beneficiary Beneficiary
	if err = json.Unmarshal(raw, &beneficiary); err != nil {
		return err
	}
	if beneficiary.Status != BeneficiaryStatusActive || (action != "DISABLE" && beneficiary.ApprovalStatus != BeneficiaryApprovalPending) || (action == "DISABLE" && beneficiary.ApprovalStatus != BeneficiaryApprovalApproved) {
		return errors.New("beneficiary not eligible for action")
	}
	var previousAction, previousActor string
	err = tx.QueryRow(`SELECT action,actor_id FROM customer_beneficiary_actions WHERE beneficiary_id=$1 ORDER BY id DESC LIMIT 1`, input.BeneficiaryID).Scan(&previousAction, &previousActor)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if action == "PROPOSE" && err != sql.ErrNoRows {
		return errors.New("approval already proposed")
	}
	if (action == "APPROVE" || action == "REJECT") && (previousAction != "PROPOSE" || previousActor == actorID) {
		return errors.New("distinct maker proposal required")
	}
	if action != "PROPOSE" && action != "APPROVE" && action != "REJECT" && action != "DISABLE" {
		return errors.New("unknown approval action")
	}
	if _, err = tx.Exec(`INSERT INTO customer_beneficiary_actions(beneficiary_id,institution_id,action,actor_id,reason) VALUES($1,$2,$3,$4,$5)`, input.BeneficiaryID, input.InstitutionID, action, actorID, strings.TrimSpace(input.Reason)); err != nil {
		return err
	}
	if action != "PROPOSE" {
		switch action {
		case "APPROVE":
			beneficiary.ApprovalStatus = BeneficiaryApprovalApproved
		case "REJECT":
			beneficiary.ApprovalStatus = BeneficiaryApprovalRejected
		case "DISABLE":
			beneficiary.Status = BeneficiaryStatusDisabled
		}
		beneficiary.UpdatedAt = time.Now().UTC()
		raw, err = json.Marshal(beneficiary)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE customer_beneficiaries SET body=$2 WHERE id=$1`, beneficiary.ID, string(raw)); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if action != "PROPOSE" {
		s.beneficiaries[beneficiary.ID] = &beneficiary
	}
	return nil
}
