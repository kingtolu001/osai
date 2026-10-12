package ledgercore

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var decimalRatePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type FXRateDirection string

const (
	SourcePerDestination FXRateDirection = "SOURCE_PER_DESTINATION"
	DestinationPerSource FXRateDirection = "DESTINATION_PER_SOURCE"
)

type FXFeeEvidence struct {
	Kind              string
	AmountMinor       int64
	Currency          string
	EvidenceReference string
}

type FXExecutionEvidence struct {
	TradeID                  string
	InstitutionID            string
	QuoteID                  string
	Executor                 string
	SourceCurrency           string
	SourceDebitMinor         int64
	DestinationCurrency      string
	DestinationProceedsMinor int64
	AppliedRate              string
	RateDirection            FXRateDirection
	ExecutionReference       string
	SourceEvidenceReference  string
	DestinationEvidenceRef   string
	Fees                     []FXFeeEvidence
	OccurredAt               time.Time
}

type FXAccount struct {
	Code  string
	Name  string
	Owner string
}

// FXJournalMapping contains the debit/credit accounts approved for posting.
// The Ledger does not infer account classifications or sides from role names.
type FXJournalMapping struct {
	SourceDebit       FXAccount
	SourceCredit      FXAccount
	DestinationDebit  FXAccount
	DestinationCredit FXAccount
}

func ValidateFXExecutionEvidence(evidence FXExecutionEvidence) error {
	if evidence.TradeID == "" || evidence.InstitutionID == "" || evidence.QuoteID == "" ||
		evidence.Executor == "" || evidence.ExecutionReference == "" ||
		evidence.SourceEvidenceReference == "" || evidence.DestinationEvidenceRef == "" {
		return errors.New("FX execution identity and independent evidence references are required")
	}
	if evidence.SourceCurrency == "" || evidence.DestinationCurrency == "" ||
		strings.EqualFold(evidence.SourceCurrency, evidence.DestinationCurrency) {
		return errors.New("FX execution requires distinct source and destination currencies")
	}
	if evidence.SourceDebitMinor <= 0 || evidence.DestinationProceedsMinor <= 0 {
		return errors.New("FX execution amounts must be positive minor units")
	}
	if evidence.RateDirection != SourcePerDestination && evidence.RateDirection != DestinationPerSource {
		return errors.New("FX rate direction is required")
	}
	rate, ok := new(big.Rat).SetString(evidence.AppliedRate)
	if len(evidence.AppliedRate) > 64 || !ok || rate.Sign() <= 0 || !decimalRatePattern.MatchString(evidence.AppliedRate) {
		return errors.New("applied FX rate must be a positive decimal")
	}
	if evidence.OccurredAt.IsZero() {
		return errors.New("FX execution time is required")
	}
	for _, fee := range evidence.Fees {
		if fee.Kind == "" || fee.AmountMinor < 0 || fee.Currency == "" ||
			(fee.AmountMinor > 0 && fee.EvidenceReference == "") {
			return fmt.Errorf("invalid FX fee evidence for kind %q", fee.Kind)
		}
	}
	return nil
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
	for _, account := range accounts {
		if account.Code == "" || account.Name == "" || account.Owner == "" {
			return nil, errors.New("complete Finance-approved FX account mapping is required")
		}
	}
	if mapping.SourceDebit.Code == mapping.SourceCredit.Code ||
		mapping.DestinationDebit.Code == mapping.DestinationCredit.Code {
		return nil, errors.New("each FX currency leg requires distinct debit and credit accounts")
	}

	source := NewJournal("jt_fx_"+evidence.TradeID+"_source", evidence.TradeID, evidence.SourceCurrency)
	source.InstitutionID = evidence.InstitutionID
	source.AddEntry(mapping.SourceDebit.Code, mapping.SourceDebit.Name, mapping.SourceDebit.Owner,
		evidence.SourceCurrency, evidence.SourceDebitMinor, 0, evidence.ExecutionReference+":source:debit")
	source.AddEntry(mapping.SourceCredit.Code, mapping.SourceCredit.Name, mapping.SourceCredit.Owner,
		evidence.SourceCurrency, 0, evidence.SourceDebitMinor, evidence.ExecutionReference+":source:credit")

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
