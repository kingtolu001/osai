package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpayout"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
)

func (r *sandboxPayoutRuntime) recoverReturn(ctx context.Context, id string) error {
	i, err := r.repository.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if i.ProviderID != "sbx:provider" || (i.State != settlementcore.Confirmed && i.State != settlementcore.Returned && i.State != settlementcore.Reversed) {
		return errors.New("confirmed sandbox payout required for return recovery")
	}
	j, err := r.repository.SandboxPayoutJob(ctx, id)
	if err != nil {
		return err
	}
	if err = r.repository.TouchSandboxPayout(ctx, id); err != nil {
		return err
	}
	var final sandboxaccounting.Command
	if !j.FinalAck || json.Unmarshal([]byte(j.FinalCommand), &final) != nil || final.Payout == nil || final.Event != "payout_complete" || !reflect.DeepEqual(*final.Payout, settlementstore.SandboxPayoutBinding(i)) {
		return errors.New("verified original payout required")
	}
	instruction := sandboxpayout.Instruction{InstitutionID: i.InstitutionID, TradeID: i.TradeID, Binding: *final.Payout}
	original, err := r.client.VerifyStored(instruction, []byte(final.EvidencePayload))
	if err != nil || original.Status != "SUCCEEDED" || original.ProviderReference != i.ProviderRef {
		return errors.New("original payout evidence mismatch")
	}
	if j.ReturnCommand == "" {
		observation, err := r.client.Query(ctx, instruction)
		if err != nil {
			return err
		}
		if observation.ProviderReference != i.ProviderRef {
			return errors.New("return provider reference differs from original payout")
		}
		if observation.Status == "SUCCEEDED" {
			return nil
		}
		event := "return"
		if observation.Status == "REVERSED" {
			event = "reversal"
		} else if observation.Status != "RETURNED" {
			return errors.New("post-payout evidence conflicts with confirmed payment")
		}
		command := settlementstore.SandboxPayoutCommand(i, final.Terms, event, final.ExpectedRevision+1, string(observation.Raw))
		frozen, err := r.repository.FreezeSandboxPayoutReturn(ctx, id, command)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(frozen)
		j.ReturnCommand = string(raw)
	}
	var returned sandboxaccounting.Command
	if json.Unmarshal([]byte(j.ReturnCommand), &returned) != nil || returned.Payout == nil || !reflect.DeepEqual(returned.Payout, final.Payout) || !reflect.DeepEqual(returned.Terms, final.Terms) || returned.ExpectedRevision != final.ExpectedRevision+1 {
		return errors.New("frozen payout return mismatch")
	}
	observation, err := r.client.VerifyStored(instruction, []byte(returned.EvidencePayload))
	if err != nil || observation.ProviderReference != i.ProviderRef {
		return errors.New("returned payout evidence mismatch")
	}
	next := settlementcore.Returned
	if returned.Event == "reversal" && observation.Status == "REVERSED" {
		next = settlementcore.Reversed
	} else if returned.Event != "return" || observation.Status != "RETURNED" {
		return errors.New("return event differs from authenticated evidence")
	}
	if err = r.post(ctx, returned, "RETURNED"); err != nil {
		return err
	}
	if err = r.repository.RecordSandboxPayoutReturn(ctx, i, next, payoutEvidence(i, observation)); err != nil {
		return err
	}
	_, err = r.repository.AdvanceSandboxPayout(ctx, id, "return")
	return err
}
