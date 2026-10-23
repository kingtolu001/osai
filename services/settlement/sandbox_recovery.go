package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/services/settlement/settlementstore"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"
)

type recoveryOperator struct {
	ActorID      string   `json:"actor_id"`
	Token        string   `json:"token"`
	Role         string   `json:"role"`
	Institutions []string `json:"institutions"`
	hash         [32]byte
}

func loadRecoveryOperators() ([]recoveryOperator, error) {
	raw := os.Getenv("OSAI_SANDBOX_RECOVERY_OPERATORS_JSON")
	if raw == "" {
		return nil, nil
	}
	var result []recoveryOperator
	if json.Unmarshal([]byte(raw), &result) != nil {
		return nil, errors.New("invalid sandbox recovery operators")
	}
	actors := map[string]bool{}
	tokens := map[[32]byte]bool{}
	for n := range result {
		v := &result[n]
		v.hash = sha256.Sum256([]byte(v.Token))
		if strings.TrimSpace(v.ActorID) == "" || actors[v.ActorID] || tokens[v.hash] || len(v.Token) < 32 || (v.Role != "maker" && v.Role != "checker") || len(v.Institutions) == 0 {
			return nil, errors.New("invalid sandbox recovery operators")
		}
		for _, institution := range v.Institutions {
			if !strings.HasPrefix(institution, "sbx:") || len(institution) <= 4 {
				return nil, errors.New("sandbox operator scope required")
			}
		}
		actors[v.ActorID] = true
		tokens[v.hash] = true
		v.Token = ""
	}
	return result, nil
}

type recoveryRequest struct {
	ID            string `json:"id"`
	InstitutionID string `json:"institution_id"`
	SettlementID  string `json:"settlement_id"`
	Action        string `json:"action"`
	Reason        string `json:"reason"`
}

func recoveryID(institution, id string) string {
	h := sha256.Sum256([]byte(institution + ":" + id))
	return hex.EncodeToString(h[:])
}

func (r *sandboxPayoutRuntime) recoveryHandler(operators []recoveryOperator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		if req.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		action := strings.TrimPrefix(req.URL.Path, "/v1/operator/sandbox-recovery/")
		if action != "propose" && action != "approve" && action != "status" {
			http.NotFound(w, req)
			return
		}
		header := req.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		candidate := sha256.Sum256([]byte(token))
		var actor *recoveryOperator
		for n := range operators {
			v := &operators[n]
			if token != header && subtle.ConstantTimeCompare(candidate[:], v.hash[:]) == 1 && (action == "status" || (action == "propose" && v.Role == "maker") || (action == "approve" && v.Role == "checker")) {
				actor = v
			}
		}
		if actor == nil {
			http.Error(w, "operator unauthorized", 401)
			return
		}
		var input recoveryRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ID == "" || len(input.ID) > 100 {
			http.Error(w, "invalid recovery request", 400)
			return
		}
		allowed := false
		for _, institution := range actor.Institutions {
			allowed = allowed || institution == input.InstitutionID
		}
		if !allowed {
			http.Error(w, "institution forbidden", 403)
			return
		}
		id := recoveryID(input.InstitutionID, input.ID)
		var err error
		switch action {
		case "propose":
			err = r.proposeRecovery(req.Context(), id, input, *actor)
		case "approve":
			err = r.approveRecovery(req.Context(), id, input.InstitutionID, actor.ActorID)
		}
		if err != nil {
			http.Error(w, "recovery action unavailable", 409)
			return
		}
		v, err := r.repository.SandboxRecovery(req.Context(), id, input.InstitutionID)
		if err != nil {
			http.Error(w, "recovery unavailable", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": input.ID, "state": v.State, "settlement_id": v.SettlementID, "next_settlement_id": v.NextSettlementID})
	})
}

func (r *sandboxPayoutRuntime) proposeRecovery(ctx context.Context, id string, input recoveryRequest, actor recoveryOperator) error {
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 500 {
		return errors.New("recovery reason required")
	}
	prior, existingErr := r.repository.SandboxRecovery(ctx, id, input.InstitutionID)
	if existingErr == nil {
		if prior.Maker == actor.ActorID && prior.SettlementID == input.SettlementID && prior.Reason == input.Reason && prior.Command.Event == input.Action {
			return nil
		}
		return errors.New("recovery proposal conflict")
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return existingErr
	}
	i, err := r.repository.GetByID(ctx, input.SettlementID)
	if err != nil {
		return err
	}
	if i.InstitutionID != input.InstitutionID || i.ProviderID != "sbx:provider" {
		return errors.New("sandbox instruction required")
	}
	p, err := r.position(ctx, i.InstitutionID, i.TradeID)
	if err != nil {
		return err
	}
	if p.Payout == nil || p.Payout.SettlementID != i.ID || !reflect.DeepEqual(*p.Payout, settlementstore.SandboxPayoutBinding(i)) {
		return errors.New("recovery payout mismatch")
	}
	want := ""
	switch input.Action {
	case "retry_payout":
		if i.State == "FAILED" && p.PayoutState == "FAILED" {
			want = "FX_PAYOUT_FAILED"
		}
	case "release_return":
		if i.State == "RETURNED" || i.State == "REVERSED" {
			want = "RETURNED"
		}
	case "refund_fee":
		if (i.State == "RETURNED" || i.State == "REVERSED") && p.Terms.FeeMinor > 0 {
			want = "RETURN_RELEASED"
		}
	}
	if want == "" || p.State != want {
		return errors.New("recovery economics or finality unavailable")
	}
	command := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, ExpectedRevision: p.Revision, EventID: "sbx:recovery:" + id, Event: input.Action, Actor: actor.ActorID, Maker: actor.ActorID, CorrelationID: i.CorrelationID, Reason: input.Reason, Terms: p.Terms}
	return r.repository.ProposeSandboxRecovery(ctx, settlementstore.SandboxRecovery{ID: id, InstitutionID: i.InstitutionID, SettlementID: i.ID, Maker: actor.ActorID, Reason: input.Reason, Command: command})
}

func (r *sandboxPayoutRuntime) approveRecovery(ctx context.Context, id, institution, checker string) error {
	v, err := r.repository.SandboxRecovery(ctx, id, institution)
	if err != nil {
		return err
	}
	if v.Maker == checker {
		return errors.New("distinct checker required")
	}
	if v.State != "PROPOSED" {
		return r.repository.ApproveSandboxRecovery(ctx, id, institution, checker, v.Command)
	}
	p, err := r.position(ctx, institution, v.Command.Terms.TradeID)
	if err != nil {
		return err
	}
	if p.Revision != v.Command.ExpectedRevision || !reflect.DeepEqual(p.Terms, v.Command.Terms) || p.Payout == nil || p.Payout.SettlementID != v.SettlementID {
		return errors.New("recovery proposal became stale")
	}
	v.Command.Checker = checker
	v.Command.Actor = checker
	payload, _ := json.Marshal(struct {
		Maker, Checker, Reason, SettlementID string
		Position                             sandboxaccounting.Position
	}{v.Maker, checker, v.Reason, v.SettlementID, p})
	hash := sha256.Sum256(payload)
	v.Command.EvidencePayload = string(payload)
	v.Command.EvidenceHash = hex.EncodeToString(hash[:])
	return r.repository.ApproveSandboxRecovery(ctx, id, institution, checker, v.Command)
}

func (r *sandboxPayoutRuntime) recoverApproved(ctx context.Context) error {
	items, err := r.repository.PendingSandboxRecoveries(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, v := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = r.repository.DB.ExecContext(ctx, `UPDATE settlement_sandbox_recovery SET attempted_at=now() WHERE id=$1`, v.ID); err != nil {
			return err
		}
		if v.Command.Event == "retry_payout" {
			if v.Command.Terms.Plan == nil {
				return errors.New("accepted retry destination required")
			}
			if approvalErr := r.currentBeneficiary(ctx, v.InstitutionID, v.Command.Terms.Plan.BeneficiaryID); approvalErr != nil {
				failures = append(failures, approvalErr)
				continue
			}
		}
		want := map[string]string{"retry_payout": "FX_HELD", "release_return": "RETURN_RELEASED", "refund_fee": "REFUNDED"}[v.Command.Event]
		if want == "" || v.Maker == v.Checker || v.Checker == "" {
			return errors.New("invalid approved recovery")
		}
		if err = r.post(ctx, v.Command, want); err == nil {
			err = r.repository.CompleteSandboxRecovery(ctx, v)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
