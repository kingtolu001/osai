package quotecore

import (
	"errors"
	"fmt"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/provider"
	"math/big"
	"strings"
)

const (
	RateScale           int64 = 100_000_000
	FeeBasisPointScale  int64 = 10_000
	maxCurrencyExponent       = 9
)

type QuoteMode = provider.QuoteMode

const (
	QuoteModeFixedSource      QuoteMode = provider.QuoteModeFixedSource
	QuoteModeFixedDestination QuoteMode = provider.QuoteModeFixedDestination
)

type FeeSchedule struct {
	ID                          string `json:"id"`
	Version                     int64  `json:"version"`
	InstitutionID               string `json:"institution_id"`
	SourceCurrency              string `json:"source_currency"`
	DestinationCurrency         string `json:"destination_currency"`
	SourceExponent              uint8  `json:"source_minor_exponent"`
	DestinationExponent         uint8  `json:"destination_minor_exponent"`
	MinimumSourcePrincipalMinor int64  `json:"minimum_source_principal_minor"`
	MaximumSourcePrincipalMinor int64  `json:"maximum_source_principal_minor"`
	MinimumTotalDebitMinor      int64  `json:"minimum_total_debit_minor"`
	MaximumTotalDebitMinor      int64  `json:"maximum_total_debit_minor"`
	MinimumDestinationMinor     int64  `json:"minimum_destination_minor"`
	MaximumDestinationMinor     int64  `json:"maximum_destination_minor"`
	FixedFeeMinor               int64  `json:"fixed_fee_minor"`
	FeeBasisPoints              int64  `json:"fee_basis_points"`
	MinimumFeeMinor             int64  `json:"minimum_fee_minor"`
	MaximumFeeMinor             int64  `json:"maximum_fee_minor"`
}

type QuotePricingInput struct {
	Mode                   QuoteMode
	SourceAmountMinor      int64
	DestinationAmountMinor int64
	RateScaled             int64
}

type PricedAmounts struct {
	SourcePrincipalMinor    int64
	FeeMinor                int64
	TotalDebitMinor         int64
	DestinationMinor        int64
	DestinationSurplusMinor int64
}

var (
	ErrPricingPolicyUnavailable = errors.New("pricing policy unavailable")
	ErrInvalidPricingPolicy     = errors.New("invalid pricing policy")
	ErrInvalidQuoteAmount       = pricing.ErrInvalidQuoteAmount
	ErrUnrepresentableDebit     = errors.New("source debit has no exact principal and fee representation")
	ErrPricingOverflow          = pricing.ErrPricingOverflow
)

func (p FeeSchedule) Validate() error {
	if strings.TrimSpace(p.ID) == "" || p.Version < 1 || strings.TrimSpace(p.InstitutionID) == "" {
		return fmt.Errorf("%w: schedule identity is required", ErrInvalidPricingPolicy)
	}
	if !validCurrencyCode(p.SourceCurrency) || !validCurrencyCode(p.DestinationCurrency) {
		return fmt.Errorf("%w: currencies must be three uppercase letters", ErrInvalidPricingPolicy)
	}
	if p.SourceExponent > maxCurrencyExponent || p.DestinationExponent > maxCurrencyExponent {
		return fmt.Errorf("%w: currency exponent is unsupported", ErrInvalidPricingPolicy)
	}
	if p.SourceCurrency == p.DestinationCurrency && p.SourceExponent != p.DestinationExponent {
		return fmt.Errorf("%w: same-currency exponents must match", ErrInvalidPricingPolicy)
	}
	if p.MinimumSourcePrincipalMinor < 1 || p.MaximumSourcePrincipalMinor < p.MinimumSourcePrincipalMinor {
		return fmt.Errorf("%w: invalid source principal limits", ErrInvalidPricingPolicy)
	}
	if p.MinimumTotalDebitMinor < 1 || p.MaximumTotalDebitMinor < p.MinimumTotalDebitMinor {
		return fmt.Errorf("%w: invalid total debit limits", ErrInvalidPricingPolicy)
	}
	if p.MinimumDestinationMinor < 1 || p.MaximumDestinationMinor < p.MinimumDestinationMinor {
		return fmt.Errorf("%w: invalid destination limits", ErrInvalidPricingPolicy)
	}
	if p.FixedFeeMinor < 0 || p.FeeBasisPoints < 0 || p.MinimumFeeMinor < 0 ||
		(p.MaximumFeeMinor > 0 && p.MaximumFeeMinor < p.MinimumFeeMinor) {
		return fmt.Errorf("%w: invalid fee terms", ErrInvalidPricingPolicy)
	}
	return nil
}

func PriceQuote(policy FeeSchedule, input QuotePricingInput) (PricedAmounts, error) {
	if err := policy.Validate(); err != nil {
		return PricedAmounts{}, err
	}
	if input.RateScaled <= 0 || (policy.SourceCurrency == policy.DestinationCurrency && input.RateScaled != RateScale) {
		return PricedAmounts{}, fmt.Errorf("%w: invalid rate for currency pair", ErrInvalidQuoteAmount)
	}

	var principal int64
	var fee int64
	switch input.Mode {
	case QuoteModeFixedSource:
		if input.SourceAmountMinor <= 0 || input.DestinationAmountMinor != 0 {
			return PricedAmounts{}, fmt.Errorf("%w: fixed-source mode requires only a positive source debit", ErrInvalidQuoteAmount)
		}
		if input.SourceAmountMinor < policy.MinimumTotalDebitMinor || input.SourceAmountMinor > policy.MaximumTotalDebitMinor {
			return PricedAmounts{}, fmt.Errorf("%w: source debit outside approved limits", ErrInvalidQuoteAmount)
		}
		var err error
		principal, fee, err = principalForExactDebit(policy, input.SourceAmountMinor)
		if err != nil {
			return PricedAmounts{}, err
		}
	case QuoteModeFixedDestination:
		if input.DestinationAmountMinor <= 0 || input.SourceAmountMinor != 0 {
			return PricedAmounts{}, fmt.Errorf("%w: fixed-destination mode requires only a positive destination amount", ErrInvalidQuoteAmount)
		}
		var err error
		principal, err = principalForDestination(policy, input.DestinationAmountMinor, input.RateScaled)
		if err != nil {
			return PricedAmounts{}, err
		}
		fee, err = feeForPrincipal(policy, principal)
		if err != nil {
			return PricedAmounts{}, err
		}
	default:
		return PricedAmounts{}, fmt.Errorf("%w: unsupported quote mode", ErrInvalidQuoteAmount)
	}

	proceeds, err := ConvertMinorUnits(principal, input.RateScaled, policy.SourceExponent, policy.DestinationExponent)
	if err != nil {
		return PricedAmounts{}, err
	}
	totalDebit, err := addMinor(principal, fee)
	if err != nil {
		return PricedAmounts{}, err
	}
	if input.Mode == QuoteModeFixedSource && totalDebit != input.SourceAmountMinor {
		return PricedAmounts{}, ErrUnrepresentableDebit
	}
	if input.Mode == QuoteModeFixedDestination {
		if totalDebit < policy.MinimumTotalDebitMinor || totalDebit > policy.MaximumTotalDebitMinor {
			return PricedAmounts{}, fmt.Errorf("%w: source debit outside approved limits", ErrInvalidQuoteAmount)
		}
		if proceeds < input.DestinationAmountMinor {
			return PricedAmounts{}, fmt.Errorf("%w: calculated proceeds underfund destination", ErrInvalidQuoteAmount)
		}
	}
	destinationForLimits := proceeds
	if input.Mode == QuoteModeFixedDestination {
		destinationForLimits = input.DestinationAmountMinor
	}
	if principal < policy.MinimumSourcePrincipalMinor || principal > policy.MaximumSourcePrincipalMinor ||
		destinationForLimits > policy.MaximumDestinationMinor || destinationForLimits < policy.MinimumDestinationMinor {
		return PricedAmounts{}, fmt.Errorf("%w: quote amount outside approved pair limits", ErrInvalidQuoteAmount)
	}
	surplus := int64(0)
	if input.Mode == QuoteModeFixedDestination {
		surplus = proceeds - input.DestinationAmountMinor
	}
	return PricedAmounts{
		SourcePrincipalMinor:    principal,
		FeeMinor:                fee,
		TotalDebitMinor:         totalDebit,
		DestinationMinor:        proceeds,
		DestinationSurplusMinor: surplus,
	}, nil
}

func principalForExactDebit(policy FeeSchedule, debit int64) (int64, int64, error) {
	if debit <= 0 {
		return 0, 0, ErrInvalidQuoteAmount
	}
	low, high := policy.MinimumSourcePrincipalMinor, policy.MaximumSourcePrincipalMinor
	for low <= high {
		mid := low + (high-low)/2
		fee, err := feeForPrincipal(policy, mid)
		if err != nil {
			return 0, 0, err
		}
		total, err := addMinor(mid, fee)
		if err != nil {
			return 0, 0, err
		}
		if total < debit {
			low = mid + 1
		} else if total > debit {
			high = mid - 1
		} else {
			return mid, fee, nil
		}
	}
	return 0, 0, ErrUnrepresentableDebit
}

func principalForDestination(policy FeeSchedule, targetMinor, rateScaled int64) (int64, error) {
	if targetMinor < policy.MinimumDestinationMinor ||
		(policy.MaximumDestinationMinor > 0 && targetMinor > policy.MaximumDestinationMinor) {
		return 0, fmt.Errorf("%w: destination amount outside approved limits", ErrInvalidQuoteAmount)
	}
	low, high := policy.MinimumSourcePrincipalMinor, policy.MaximumSourcePrincipalMinor
	var candidate int64
	for low <= high {
		mid := low + (high-low)/2
		proceeds, err := ConvertMinorUnits(mid, rateScaled, policy.SourceExponent, policy.DestinationExponent)
		if err != nil {
			return 0, err
		}
		if proceeds >= targetMinor {
			candidate = mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	if candidate == 0 {
		return 0, fmt.Errorf("%w: destination amount cannot be funded within approved source limits", ErrInvalidQuoteAmount)
	}
	return candidate, nil
}

func feeForPrincipal(policy FeeSchedule, principal int64) (int64, error) {
	percentage := new(big.Int).Mul(big.NewInt(principal), big.NewInt(policy.FeeBasisPoints))
	percentage.Add(percentage, big.NewInt(FeeBasisPointScale-1))
	percentage.Quo(percentage, big.NewInt(FeeBasisPointScale))
	fee := percentage.Add(percentage, big.NewInt(policy.FixedFeeMinor))
	if policy.MinimumFeeMinor > 0 && fee.Cmp(big.NewInt(policy.MinimumFeeMinor)) < 0 {
		fee.SetInt64(policy.MinimumFeeMinor)
	}
	if policy.MaximumFeeMinor > 0 && fee.Cmp(big.NewInt(policy.MaximumFeeMinor)) > 0 {
		fee.SetInt64(policy.MaximumFeeMinor)
	}
	if !fee.IsInt64() {
		return 0, ErrPricingOverflow
	}
	return fee.Int64(), nil
}

func ConvertMinorUnits(sourceMinor, rateScaled int64, sourceExponent, destinationExponent uint8) (int64, error) {
	return pricing.ConvertMinorUnits(sourceMinor, rateScaled, sourceExponent, destinationExponent)
}

func addMinor(left, right int64) (int64, error) {
	sum := new(big.Int).Add(big.NewInt(left), big.NewInt(right))
	if !sum.IsInt64() {
		return 0, ErrPricingOverflow
	}
	return sum.Int64(), nil
}

func pow10(exponent uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

func validCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
