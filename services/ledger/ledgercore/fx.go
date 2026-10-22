package ledgercore

import (
	"errors"
	"fmt"

	"github.com/osai/osai/pkg/fxevidence"
)

type FXRateDirection = fxevidence.FXRateDirection

const (
	SourcePerDestination = fxevidence.SourcePerDestination
	DestinationPerSource = fxevidence.DestinationPerSource
)

type FXFeeEvidence = fxevidence.FXFeeEvidence
type FXExecutionEvidence = fxevidence.FXExecutionEvidence

func ValidateFXExecutionEvidence(evidence FXExecutionEvidence) error {
	return fxevidence.ValidateFXExecutionEvidence(evidence)
}

type FXAccount struct {
	Code     string
	Name     string
	Owner    string
	Currency string
}

// FXJournalMapping supplies explicit debit/credit accounts for the FX legs.
// Approval and account classification are deployment concerns; the Ledger
// does not infer them from role names.
type FXJournalMapping struct {
	SourceDebit       FXAccount
	SourceCredit      FXAccount
	DestinationDebit  FXAccount
	DestinationCredit FXAccount
}

func BuildFXExecutionJournals(evidence FXExecutionEvidence, mapping FXJournalMapping) ([]Journal, error) {
	if err := ValidateFXExecutionEvidence(evidence); err != nil {
		return nil, err
	}
	accounts := []FXAccount{
		mapping.SourceDebit,
		mapping.SourceCredit,
		mapping.DestinationDebit,
		mapping.DestinationCredit,
	}
	for index, account := range accounts {
		if account.Code == "" || account.Name == "" || account.Owner == "" {
			return nil, errors.New("complete explicit FX account mapping is required")
		}
		expectedCurrency := evidence.SourceCurrency
		if index >= 2 {
			expectedCurrency = evidence.DestinationCurrency
		}
		if account.Currency != expectedCurrency {
			return nil, fmt.Errorf("FX account %q currency %q does not match journal currency %q", account.Code, account.Currency, expectedCurrency)
		}
	}
	if mapping.SourceDebit.Code == mapping.SourceCredit.Code ||
		mapping.DestinationDebit.Code == mapping.DestinationCredit.Code {
		return nil, errors.New("each FX currency leg requires distinct debit and credit accounts")
	}

	source := NewJournal("jt_fx_"+evidence.TradeID+"_source", evidence.TradeID, evidence.SourceCurrency)
	source.InstitutionID = evidence.InstitutionID
	source.AddEntry(mapping.SourceDebit.Code, mapping.SourceDebit.Name, mapping.SourceDebit.Owner,
		evidence.SourceCurrency, evidence.SourcePrincipalMinor, 0, evidence.ExecutionReference+":source:debit")
	source.AddEntry(mapping.SourceCredit.Code, mapping.SourceCredit.Name, mapping.SourceCredit.Owner,
		evidence.SourceCurrency, 0, evidence.SourcePrincipalMinor, evidence.ExecutionReference+":source:credit")

	destination := NewJournal("jt_fx_"+evidence.TradeID+"_destination", evidence.TradeID, evidence.DestinationCurrency)
	destination.InstitutionID = evidence.InstitutionID
	destination.AddEntry(mapping.DestinationDebit.Code, mapping.DestinationDebit.Name, mapping.DestinationDebit.Owner,
		evidence.DestinationCurrency, evidence.DestinationProceedsMinor, 0, evidence.ExecutionReference+":destination:debit")
	destination.AddEntry(mapping.DestinationCredit.Code, mapping.DestinationCredit.Name, mapping.DestinationCredit.Owner,
		evidence.DestinationCurrency, 0, evidence.DestinationProceedsMinor, evidence.ExecutionReference+":destination:credit")

	if err := source.Validate(); err != nil {
		return nil, fmt.Errorf("validate source-currency FX journal: %w", err)
	}
	if err := destination.Validate(); err != nil {
		return nil, fmt.Errorf("validate destination-currency FX journal: %w", err)
	}
	return []Journal{*source, *destination}, nil
}
