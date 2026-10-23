// Package sandboxaccounting defines the fixture-only Ledger command contract.
// It conveys no production account mapping or authority to move real money.
package sandboxaccounting

import (
	"encoding/hex"
	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
	"strings"
)

const Version = "osai-sandbox-accounting/v1"

// ValidBeneficiaryID preserves Customer-owned identifiers. It validates shape,
// not approval or sandbox authority; callers must also verify institution,
// runtime profile and the Customer service's approval response.
func ValidBeneficiaryID(id string) bool {
	if strings.HasPrefix(id, "sbx:") && len(id) > 4 {
		return true
	}
	if !strings.HasPrefix(id, "ben_") {
		return false
	}
	_, err := uuid.Parse(strings.TrimPrefix(id, "ben_"))
	return err == nil
}

func FixtureQuoteID(id string) bool {
	if strings.HasPrefix(id, "sbx:") && len(id) > 4 {
		return true
	}
	if !strings.HasPrefix(id, "quo_") {
		return false
	}
	value, err := hex.DecodeString(strings.TrimPrefix(id, "quo_"))
	return err == nil && len(value) == 6
}

// FXPlan contains accepted economics, without claiming execution or custody.
type FXPlan struct {
	QuoteID, Executor, AppliedRate string
	DestinationMinor               int64
	RateDirection                  fxevidence.FXRateDirection
	BeneficiaryID                  string `json:",omitempty"`
	ProviderID                     string `json:",omitempty"`
}

type Terms struct {
	TradeID, InstitutionID   string
	PrincipalMinor, FeeMinor int64
	// FX preserves the original manifest fixture contract. New runtime callers
	// use Plan and supply actual execution evidence only on fx_complete.
	FX   *fxevidence.FXExecutionEvidence
	Plan *FXPlan `json:",omitempty"`
}

type Command struct {
	ExpectedRevision                                            int64 `json:",omitempty"`
	PolicyVersion, EventID, Event, Actor, CorrelationID, Reason string
	EvidenceHash, Maker, Checker                                string
	EvidencePayload                                             string
	Terms                                                       Terms
	Execution                                                   *fxevidence.FXExecutionEvidence `json:",omitempty"`
	Payout                                                      *PayoutBinding                  `json:",omitempty"`
}

// PayoutBinding identifies one fixture payout attempt against already posted
// destination proceeds. It is separate from immutable quote economics.
type PayoutBinding struct {
	SettlementID, ClientReference, ProviderID, BeneficiaryID string
	Currency                                                 string
	AmountMinor                                              int64
}

type Position struct {
	Terms       Terms
	State       string
	Revision    int64
	Payout      *PayoutBinding `json:",omitempty"`
	PayoutState string         `json:",omitempty"`
}
