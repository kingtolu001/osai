package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpayout"
	"github.com/osai/osai/pkg/sandboxpolicy"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type sandboxPayoutRuntime struct {
	repository    settlementstore.Postgres
	ledger        ledgerv1.LedgerServiceClient
	client        *sandboxpayout.Client
	beneficiaries beneficiaryVerifier
}

func startSandboxPayout(ctx context.Context, repository settlementstore.Postgres, verifiers ...beneficiaryVerifier) (*sandboxPayoutRuntime, error) {
	enabled, err := sandboxpolicy.FXPayout()
	if err != nil || !enabled {
		return nil, err
	}
	host, _, err := net.SplitHostPort(os.Getenv("OSAI_LEDGER_GRPC_ADDR"))
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return nil, errors.New("sandbox payout requires explicit loopback Ledger target")
	}
	c, err := sandboxpayout.New(os.Getenv("OSAI_SANDBOX_PAYOUT_BASE_URL"), os.Getenv("OSAI_SANDBOX_PAYOUT_SIGNING_KEY"))
	if err != nil {
		return nil, err
	}
	if err = repository.EnsureSandboxPayoutSchema(ctx); err != nil {
		return nil, err
	}
	if err = repository.EnsureSandboxRecoverySchema(ctx); err != nil {
		return nil, err
	}
	alertAge, err := sandboxPayoutAlertAge()
	if err != nil {
		return nil, err
	}
	registration, err := registerSandboxPayoutMetrics(repository)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(os.Getenv("OSAI_LEDGER_GRPC_ADDR"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = registration.Unregister()
		return nil, err
	}
	var verifier beneficiaryVerifier
	if len(verifiers) == 1 {
		verifier = verifiers[0]
	}
	r := &sandboxPayoutRuntime{repository: repository, ledger: ledgerv1.NewLedgerServiceClient(conn), client: c, beneficiaries: verifier}
	go func() {
		defer conn.Close()
		defer registration.Unregister()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			batch, cancel := context.WithTimeout(ctx, 10*time.Second)
			ids, err := repository.PendingSandboxPayouts(batch, 25)
			if err == nil {
				for _, id := range ids {
					if batch.Err() != nil {
						break
					}
					if callErr := r.process(batch, id); callErr != nil {
						log.Printf("sandbox payout remains unresolved: settlement_id=%s", id)
					}
				}
			}
			if err != nil {
				log.Printf("sandbox payout recovery scan unavailable")
			}
			cancel()
			returnContext, returnCancel := context.WithTimeout(ctx, 10*time.Second)
			returns, returnErr := repository.PendingSandboxPayoutReturns(returnContext, 25)
			if returnErr == nil {
				for _, id := range returns {
					if returnContext.Err() != nil {
						break
					}
					if err := r.recoverReturn(returnContext, id); err != nil {
						log.Printf("sandbox payout return remains unresolved: settlement_id=%s", id)
					}
				}
			} else {
				log.Printf("sandbox payout return scan unavailable")
			}
			returnCancel()
			// A slow provider must not consume the separate alerting budget.
			alertContext, alertCancel := context.WithTimeout(ctx, 2*time.Second)
			if err := repository.EscalateSandboxUnknownPayouts(alertContext, alertAge, 25); err != nil {
				log.Printf("sandbox payout UNKNOWN escalation unavailable")
			}
			alertCancel()
			recoveryContext, recoveryCancel := context.WithTimeout(ctx, 10*time.Second)
			if err := r.recoverApproved(recoveryContext); err != nil {
				log.Printf("approved sandbox recovery remains unresolved")
			}
			recoveryCancel()
		}
	}()
	return r, nil
}

func (r *sandboxPayoutRuntime) position(ctx context.Context, institution, trade string) (sandboxaccounting.Position, error) {
	response, err := r.ledger.GetSandboxPosition(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxPositionRequest{InstitutionId: institution, TradeId: trade})
	var p sandboxaccounting.Position
	if err != nil {
		return p, err
	}
	if response == nil || json.Unmarshal([]byte(response.PositionJson), &p) != nil || p.Terms.InstitutionID != institution || p.Terms.TradeID != trade {
		return p, errors.New("sandbox Ledger position mismatch")
	}
	return p, nil
}

func (r *sandboxPayoutRuntime) post(ctx context.Context, c sandboxaccounting.Command, wantState string) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	receipt, err := r.ledger.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(raw)})
	if err != nil {
		return err
	}
	if receipt == nil || receipt.EventId != c.EventID || receipt.State != wantState || receipt.Revision != c.ExpectedRevision+1 {
		return errors.New("sandbox payout Ledger receipt mismatch")
	}
	return nil
}

func (r *sandboxPayoutRuntime) process(ctx context.Context, id string) error {
	i, err := r.repository.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if i.ProviderID != "sbx:provider" || !strings.HasPrefix(i.InstitutionID, "sbx:") {
		return errors.New("sandbox payout instruction required")
	}
	j, err := r.repository.SandboxPayoutJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		p, readErr := r.position(ctx, i.InstitutionID, i.TradeID)
		if readErr != nil {
			return readErr
		}
		j, err = r.repository.PrepareSandboxPayout(ctx, i, p)
	}
	if err != nil {
		return err
	}
	if err = r.repository.TouchSandboxPayout(ctx, id); err != nil {
		return err
	}
	var bind sandboxaccounting.Command
	if json.Unmarshal([]byte(j.BindCommand), &bind) != nil || bind.Payout == nil || !reflect.DeepEqual(*bind.Payout, settlementstore.SandboxPayoutBinding(i)) || bind.Terms.TradeID != i.TradeID || bind.Terms.InstitutionID != i.InstitutionID || bind.ExpectedRevision < 1 || bind.ExpectedRevision > 9223372036854775804 {
		return errors.New("frozen sandbox payout command mismatch")
	}
	if !j.BindAck {
		if err = r.post(ctx, bind, "FX_HELD"); err != nil {
			return err
		}
		if _, err = r.repository.AdvanceSandboxPayout(ctx, id, "bind"); err != nil {
			return err
		}
	}
	if !j.SubmitAck {
		submit := settlementstore.SandboxPayoutCommand(i, bind.Terms, "payout_submitted", bind.ExpectedRevision+1, bind.EvidencePayload)
		if err = r.post(ctx, submit, "FX_HELD"); err != nil {
			return err
		}
		if _, err = r.repository.AdvanceSandboxPayout(ctx, id, "submit"); err != nil {
			return err
		}
	}
	instruction := sandboxpayout.Instruction{InstitutionID: i.InstitutionID, TradeID: i.TradeID, Binding: *bind.Payout}
	if j.FinalCommand == "" {
		if i.Supersedes != "" && !j.DispatchStarted {
			if err := r.currentBeneficiary(ctx, i.InstitutionID, i.Beneficiary); err != nil {
				return err
			}
		}
		if err = r.repository.RecordRuntimeState(ctx, id, settlementcore.Submitted, "", provider.Evidence{}); err != nil {
			return err
		}
		won, err := r.repository.AdvanceSandboxPayout(ctx, id, "dispatch")
		if err != nil {
			return err
		}
		if won {
			// The fence is committed before POST. Even a crash before the call
			// leaves query-first recovery; NOT_FOUND never authorizes resubmission.
			_ = r.client.Submit(ctx, instruction)
		}
		observation, err := r.client.Query(ctx, instruction)
		if err != nil {
			return errors.Join(err, r.repository.RecordRuntimeState(ctx, id, settlementcore.Unknown, "", provider.Evidence{}))
		}
		if i.ProviderRef != "" && observation.ProviderReference != "" && i.ProviderRef != observation.ProviderReference {
			return errors.New("sandbox payout provider reference changed")
		}
		switch observation.Status {
		case "UNKNOWN", "NOT_FOUND":
			return r.repository.RecordRuntimeState(ctx, id, settlementcore.Unknown, observation.ProviderReference, payoutEvidence(i, observation))
		case "PENDING":
			return r.repository.RecordRuntimeState(ctx, id, settlementcore.Processing, observation.ProviderReference, payoutEvidence(i, observation))
		case "SUCCEEDED", "FAILED":
			event := "payout_complete"
			if observation.Status == "FAILED" {
				event = "payout_failed"
			}
			command := settlementstore.SandboxPayoutCommand(i, bind.Terms, event, bind.ExpectedRevision+2, string(observation.Raw))
			frozen, err := r.repository.FreezeSandboxPayoutFinal(ctx, id, command)
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(frozen)
			j.FinalCommand = string(raw)
		default:
			return errors.New("sandbox payout return requires a previously confirmed payment")
		}
	}
	var final sandboxaccounting.Command
	if json.Unmarshal([]byte(j.FinalCommand), &final) != nil || final.Payout == nil || !reflect.DeepEqual(final.Payout, bind.Payout) || !reflect.DeepEqual(final.Terms, bind.Terms) || final.ExpectedRevision != bind.ExpectedRevision+2 {
		return errors.New("frozen payout finality mismatch")
	}
	observation, err := r.client.VerifyStored(instruction, []byte(final.EvidencePayload))
	if err != nil {
		return err
	}
	wantState, next := "COMPLETE", settlementcore.Confirmed
	if final.Event == "payout_failed" && observation.Status == "FAILED" {
		wantState, next = "FX_PAYOUT_FAILED", settlementcore.Failed
	} else if final.Event != "payout_complete" || observation.Status != "SUCCEEDED" {
		return errors.New("payout finality differs from authenticated observation")
	}
	if err = r.post(ctx, final, wantState); err != nil {
		return err
	}
	if _, err = r.repository.AdvanceSandboxPayout(ctx, id, "final"); err != nil {
		return err
	}
	return r.repository.RecordRuntimeState(ctx, id, next, observation.ProviderReference, payoutEvidence(i, observation))
}

func (r *sandboxPayoutRuntime) currentBeneficiary(ctx context.Context, institution, id string) error {
	if r.beneficiaries == nil {
		return errors.New("Customer approval boundary required for a new payout attempt")
	}
	approved, err := r.beneficiaries.GetApprovedBeneficiary(ctx, &customerv1.BeneficiaryRequest{InstitutionId: institution, BeneficiaryId: id})
	if err != nil || approved == nil || approved.BeneficiaryId != id || approved.InstitutionId != institution || !strings.EqualFold(approved.Status, "ACTIVE") || !strings.EqualFold(approved.ApprovalStatus, "APPROVED") {
		return errors.New("beneficiary no longer approved for a new payout attempt")
	}
	return nil
}

func payoutEvidence(i settlementcore.Instruction, o sandboxpayout.Observation) provider.Evidence {
	hash := sha256.Sum256(o.Raw)
	digest := hex.EncodeToString(hash[:])
	return provider.Evidence{ID: "sbx:payout-evidence:" + digest, Provider: "sbx:provider", Method: "authenticated_query", RequestRef: i.ClientRef, RawPayloadHash: digest, ParsedAt: o.ObservedAt, CorrelationID: i.CorrelationID}
}
