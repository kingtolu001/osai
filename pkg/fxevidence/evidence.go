package fxevidence

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
	SourceExponent           uint8
	SourcePrincipalMinor     int64
	DestinationCurrency      string
	DestinationExponent      uint8
	DestinationProceedsMinor int64
	AppliedRate              string
	RateDirection            FXRateDirection
	ExecutionReference       string
	SourceEvidenceReference  string
	DestinationEvidenceRef   string
	Fees                     []FXFeeEvidence
	OccurredAt               time.Time
}

func ValidateFXExecutionEvidence(evidence FXExecutionEvidence) error {
	if evidence.TradeID == "" || evidence.InstitutionID == "" || evidence.QuoteID == "" ||
		evidence.Executor == "" || evidence.ExecutionReference == "" ||
		evidence.SourceEvidenceReference == "" || evidence.DestinationEvidenceRef == "" {
		return errors.New("FX execution identity and independent evidence references are required")
	}
	if !validFXCurrency(evidence.SourceCurrency) || !validFXCurrency(evidence.DestinationCurrency) ||
		strings.EqualFold(evidence.SourceCurrency, evidence.DestinationCurrency) {
		return errors.New("FX execution requires distinct source and destination currencies")
	}
	if evidence.SourceExponent > 9 || evidence.DestinationExponent > 9 {
		return errors.New("FX currency exponent is unsupported")
	}
	if evidence.SourcePrincipalMinor <= 0 || evidence.DestinationProceedsMinor <= 0 {
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
			(!strings.EqualFold(fee.Currency, evidence.SourceCurrency) && !strings.EqualFold(fee.Currency, evidence.DestinationCurrency)) ||
			(fee.AmountMinor > 0 && fee.EvidenceReference == "") {
			return fmt.Errorf("invalid FX fee evidence for kind %q", fee.Kind)
		}
	}
	expectedProceeds, err := fxDestinationProceeds(evidence)
	if err != nil {
		return err
	}
	if expectedProceeds != evidence.DestinationProceedsMinor {
		return fmt.Errorf("FX proceeds mismatch: applied rate yields %d minor units, evidence reports %d", expectedProceeds, evidence.DestinationProceedsMinor)
	}
	return nil
}

func fxDestinationProceeds(evidence FXExecutionEvidence) (int64, error) {
	rate, ok := new(big.Rat).SetString(evidence.AppliedRate)
	if !ok || rate.Sign() <= 0 {
		return 0, errors.New("invalid applied FX rate")
	}
	numerator := new(big.Int).Set(big.NewInt(evidence.SourcePrincipalMinor))
	denominator := new(big.Int)
	switch evidence.RateDirection {
	case SourcePerDestination:
		numerator.Mul(numerator, rate.Denom())
		denominator.Set(rate.Num())
		numerator.Mul(numerator, pow10FX(evidence.DestinationExponent))
		denominator.Mul(denominator, pow10FX(evidence.SourceExponent))
	case DestinationPerSource:
		numerator.Mul(numerator, rate.Num())
		denominator.Set(rate.Denom())
		numerator.Mul(numerator, pow10FX(evidence.DestinationExponent))
		denominator.Mul(denominator, pow10FX(evidence.SourceExponent))
	default:
		return 0, errors.New("FX rate direction is required")
	}
	if denominator.Sign() <= 0 {
		return 0, errors.New("invalid FX conversion denominator")
	}
	proceeds := numerator.Quo(numerator, denominator)
	if !proceeds.IsInt64() {
		return 0, errors.New("FX destination proceeds exceed supported integer range")
	}
	return proceeds.Int64(), nil
}

func pow10FX(exponent uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

func validFXCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, character := range currency {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}
