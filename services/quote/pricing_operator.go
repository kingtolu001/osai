package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/services/quote/quotecore"
)

type pricingOperator struct {
	ActorID      string   `json:"actor_id"`
	Token        string   `json:"token"`
	Role         string   `json:"role"`
	Institutions []string `json:"institutions"`
	tokenHash    [32]byte
}

type pricingScheduleProposalRequest struct {
	Policy quotecore.FeeSchedule `json:"fee_schedule"`
	Reason string                `json:"reason"`
}

type pricingScheduleDecisionRequest struct {
	ProposalID    string `json:"proposal_id"`
	InstitutionID string `json:"institution_id"`
	Reason        string `json:"reason"`
}

func loadPricingOperators() ([]pricingOperator, error) {
	raw := strings.TrimSpace(os.Getenv("OSAI_QUOTE_PRICING_OPERATORS_JSON"))
	if raw == "" {
		return nil, nil
	}
	var operators []pricingOperator
	if err := json.Unmarshal([]byte(raw), &operators); err != nil {
		return nil, errors.New("invalid quote pricing operator configuration")
	}
	actors, tokens := map[string]bool{}, map[[32]byte]bool{}
	hasMaker, hasChecker := false, false
	for i := range operators {
		operator := &operators[i]
		operator.ActorID = strings.TrimSpace(operator.ActorID)
		if operator.ActorID == "" || len(operator.Token) < 32 ||
			(operator.Role != "maker" && operator.Role != "checker") ||
			len(operator.Institutions) == 0 || actors[operator.ActorID] {
			return nil, errors.New("invalid quote pricing operator configuration")
		}
		for index := range operator.Institutions {
			operator.Institutions[index] = strings.TrimSpace(operator.Institutions[index])
			if operator.Institutions[index] == "" {
				return nil, errors.New("invalid quote pricing operator configuration")
			}
		}
		operator.tokenHash = sha256.Sum256([]byte(operator.Token))
		if tokens[operator.tokenHash] {
			return nil, errors.New("duplicate quote pricing operator token")
		}
		actors[operator.ActorID], tokens[operator.tokenHash] = true, true
		hasMaker = hasMaker || operator.Role == "maker"
		hasChecker = hasChecker || operator.Role == "checker"
		operator.Token = ""
	}
	if !hasMaker || !hasChecker {
		return nil, errors.New("quote pricing requires both maker and checker operators")
	}
	return operators, nil
}

func servePricingAdmin(address, certificate, privateKey string, handler http.Handler) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid quote pricing admin address: %w", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if (certificate == "") != (privateKey == "") {
		return errors.New("both quote pricing admin TLS certificate and key are required")
	}
	if !loopback && certificate == "" {
		return errors.New("TLS is required for non-loopback quote pricing admin listener")
	}
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if certificate != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		return server.ListenAndServeTLS(certificate, privateKey)
	}
	return server.ListenAndServe()
}

func (s *grpcServer) pricingOperatorHandler(operators []pricingOperator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/operator/pricing/propose", s.proposePricingSchedule(operators))
	mux.HandleFunc("/v1/operator/pricing/approve", s.decidePricingSchedule(operators, true))
	mux.HandleFunc("/v1/operator/pricing/reject", s.decidePricingSchedule(operators, false))
	return mux
}

func (s *grpcServer) proposePricingSchedule(operators []pricingOperator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		var request pricingScheduleProposalRequest
		if err := decodeStrictJSON(r.Body, &request); err != nil {
			http.Error(w, "invalid pricing proposal", http.StatusBadRequest)
			return
		}
		request.Policy.ID = ""
		request.Policy.Version = 0
		operator := authenticatePricingOperator(r, operators, "maker", request.Policy.InstitutionID)
		if operator == nil {
			http.Error(w, "operator unauthorized", http.StatusUnauthorized)
			return
		}
		proposalID, err := s.store.ProposeFeeSchedule(r.Context(), request.Policy, operator.ActorID, request.Reason)
		if err != nil {
			http.Error(w, "pricing proposal unavailable", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"proposal_id": proposalID})
	}
}

func (s *grpcServer) decidePricingSchedule(operators []pricingOperator, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var request pricingScheduleDecisionRequest
		if err := decodeStrictJSON(r.Body, &request); err != nil ||
			strings.TrimSpace(request.ProposalID) == "" ||
			strings.TrimSpace(request.InstitutionID) == "" ||
			strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 500 {
			http.Error(w, "invalid pricing decision", http.StatusBadRequest)
			return
		}
		operator := authenticatePricingOperator(r, operators, "checker", request.InstitutionID)
		if operator == nil {
			http.Error(w, "operator unauthorized", http.StatusUnauthorized)
			return
		}
		var err error
		if approve {
			err = s.store.ApproveFeeSchedule(r.Context(), request.ProposalID, request.InstitutionID, operator.ActorID, request.Reason)
		} else {
			err = s.store.RejectFeeSchedule(r.Context(), request.ProposalID, request.InstitutionID, operator.ActorID, request.Reason)
		}
		if err != nil {
			http.Error(w, "pricing decision unavailable", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func authenticatePricingOperator(r *http.Request, operators []pricingOperator, role, institutionID string) *pricingOperator {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" {
		return nil
	}
	candidate := sha256.Sum256([]byte(token))
	for i := range operators {
		operator := &operators[i]
		if operator.Role != role || subtle.ConstantTimeCompare(candidate[:], operator.tokenHash[:]) != 1 {
			continue
		}
		for _, allowedInstitution := range operator.Institutions {
			if allowedInstitution == institutionID {
				return operator
			}
		}
	}
	return nil
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
