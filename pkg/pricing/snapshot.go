package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

const (
	RateScale           int64 = 100_000_000
	FeeBasisPointScale  int64 = 10_000
	maxCurrencyExponent       = 9
)

type Snapshot struct {
	Mode                    string `json:"mode"`
	RequestedAmountMinor    int64  `json:"requested_amount_minor"`
	SourcePrincipalMinor    int64  `json:"source_principal_minor"`
	FeeMinor                int64  `json:"fee_minor"`
	TotalDebitMinor         int64  `json:"total_debit_minor"`
	DestinationAmountMinor  int64  `json:"destination_amount_minor"`
	DestinationTargetMinor  int64  `json:"destination_target_minor"`
	DestinationSurplusMinor int64  `json:"destination_surplus_minor"`
	SourceCurrency          string `json:"source_currency"`
	DestinationCurrency     string `json:"destination_currency"`
	RateScaled              int64  `json:"rate_scaled"`
	RateDirection           string `json:"rate_direction"`
	RateScale               int64  `json:"rate_scale"`
	SourceExponent          uint32 `json:"source_minor_exponent"`
	DestinationExponent     uint32 `json:"destination_minor_exponent"`
	FeeScheduleID           string `json:"fee_schedule_id"`
	FeeScheduleVersion      int64  `json:"fee_schedule_version"`
	FixedFeeMinor           int64  `json:"fixed_fee_minor"`
	FeeBasisPoints          int64  `json:"fee_basis_points"`
	MinimumFeeMinor         int64  `json:"minimum_fee_minor"`
	MaximumFeeMinor         int64  `json:"maximum_fee_minor"`
	MinimumPrincipalMinor   int64  `json:"minimum_source_principal_minor"`
	MaximumPrincipalMinor   int64  `json:"maximum_source_principal_minor"`
	MinimumDebitMinor       int64  `json:"minimum_total_debit_minor"`
	MaximumDebitMinor       int64  `json:"maximum_total_debit_minor"`
	MinimumDestinationMinor int64  `json:"minimum_destination_minor"`
	MaximumDestinationMinor int64  `json:"maximum_destination_minor"`
	ExpiresAt               string `json:"expires_at"`
	Hash                    string `json:"hash"`
}

func Hash(snapshot Snapshot) (string, error) {
	snapshot.Hash = ""
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func Validate(snapshot Snapshot) error {
	if strings.TrimSpace(snapshot.FeeScheduleID) == "" || snapshot.FeeScheduleVersion < 1 ||
		(snapshot.Mode != "FIXED_SOURCE" && snapshot.Mode != "FIXED_DESTINATION") {
		return errors.New("pricing snapshot identity or mode is invalid")
	}
	if !validCurrency(snapshot.SourceCurrency) || !validCurrency(snapshot.DestinationCurrency) ||
		snapshot.SourceExponent > maxCurrencyExponent || snapshot.DestinationExponent > maxCurrencyExponent ||
		snapshot.RateScaled <= 0 || snapshot.RateScale != RateScale ||
		snapshot.RateDirection != snapshot.DestinationCurrency+"_PER_"+snapshot.SourceCurrency {
		return errors.New("pricing snapshot currency or rate terms are invalid")
	}
	if snapshot.MinimumPrincipalMinor <= 0 || snapshot.MaximumPrincipalMinor < snapshot.MinimumPrincipalMinor ||
		snapshot.MinimumDebitMinor <= 0 || snapshot.MaximumDebitMinor < snapshot.MinimumDebitMinor ||
		snapshot.MinimumDestinationMinor <= 0 || snapshot.MaximumDestinationMinor < snapshot.MinimumDestinationMinor ||
		snapshot.FixedFeeMinor < 0 || snapshot.FeeBasisPoints < 0 || snapshot.MinimumFeeMinor < 0 ||
		(snapshot.MaximumFeeMinor > 0 && snapshot.MaximumFeeMinor < snapshot.MinimumFeeMinor) {
		return errors.New("pricing snapshot limits or fee terms are invalid")
	}
	if snapshot.SourceCurrency == snapshot.DestinationCurrency &&
		(snapshot.SourceExponent != snapshot.DestinationExponent || snapshot.RateScaled != RateScale) {
		return errors.New("same-currency pricing snapshot must use identity conversion")
	}
	if snapshot.SourcePrincipalMinor < snapshot.MinimumPrincipalMinor ||
		snapshot.SourcePrincipalMinor > snapshot.MaximumPrincipalMinor ||
		snapshot.TotalDebitMinor < snapshot.MinimumDebitMinor ||
		snapshot.TotalDebitMinor > snapshot.MaximumDebitMinor {
		return errors.New("pricing snapshot amounts are outside schedule limits")
	}
	total := new(big.Int).Add(big.NewInt(snapshot.SourcePrincipalMinor), big.NewInt(snapshot.FeeMinor))
	if snapshot.FeeMinor < 0 || !total.IsInt64() || snapshot.TotalDebitMinor != total.Int64() {
		return errors.New("pricing snapshot debit does not equal principal plus fee")
	}
	expectedFee, err := calculateFee(snapshot)
	if err != nil || expectedFee != snapshot.FeeMinor {
		return errors.New("pricing snapshot fee does not match its schedule")
	}
	expectedDestination, err := convert(snapshot)
	if err != nil || expectedDestination != snapshot.DestinationAmountMinor {
		return errors.New("pricing snapshot destination does not match its rate")
	}
	switch snapshot.Mode {
	case "FIXED_SOURCE":
		if snapshot.RequestedAmountMinor != snapshot.TotalDebitMinor ||
			snapshot.DestinationTargetMinor != 0 || snapshot.DestinationSurplusMinor != 0 ||
			snapshot.DestinationAmountMinor < snapshot.MinimumDestinationMinor ||
			snapshot.DestinationAmountMinor > snapshot.MaximumDestinationMinor {
			return errors.New("fixed-source pricing snapshot is inconsistent")
		}
	case "FIXED_DESTINATION":
		if snapshot.RequestedAmountMinor <= 0 ||
			snapshot.RequestedAmountMinor != snapshot.DestinationTargetMinor ||
			snapshot.RequestedAmountMinor < snapshot.MinimumDestinationMinor ||
			snapshot.RequestedAmountMinor > snapshot.MaximumDestinationMinor ||
			snapshot.DestinationSurplusMinor != snapshot.DestinationAmountMinor-snapshot.DestinationTargetMinor ||
			snapshot.DestinationSurplusMinor < 0 {
			return errors.New("fixed-destination pricing snapshot is inconsistent")
		}
		if snapshot.SourcePrincipalMinor > snapshot.MinimumPrincipalMinor {
			previous := snapshot
			previous.SourcePrincipalMinor--
			proceeds, conversionErr := convert(previous)
			if conversionErr != nil || proceeds >= snapshot.DestinationTargetMinor {
				return errors.New("fixed-destination principal is not minimal")
			}
		}
	default:
		return errors.New("unsupported pricing mode")
	}
	if _, err := time.Parse(time.RFC3339Nano, snapshot.ExpiresAt); err != nil {
		return errors.New("pricing snapshot expiry is invalid")
	}
	hash, err := Hash(snapshot)
	if err != nil || snapshot.Hash == "" || snapshot.Hash != hash {
		return errors.New("pricing snapshot hash mismatch")
	}
	return nil
}

func calculateFee(snapshot Snapshot) (int64, error) {
	percentage := new(big.Int).Mul(big.NewInt(snapshot.SourcePrincipalMinor), big.NewInt(snapshot.FeeBasisPoints))
	percentage.Add(percentage, big.NewInt(FeeBasisPointScale-1))
	percentage.Quo(percentage, big.NewInt(FeeBasisPointScale))
	fee := new(big.Int).Add(percentage, big.NewInt(snapshot.FixedFeeMinor))
	if !fee.IsInt64() {
		return 0, errors.New("pricing snapshot fee overflows")
	}
	value := fee.Int64()
	if value < snapshot.MinimumFeeMinor {
		value = snapshot.MinimumFeeMinor
	}
	if snapshot.MaximumFeeMinor > 0 && value > snapshot.MaximumFeeMinor {
		value = snapshot.MaximumFeeMinor
	}
	return value, nil
}

func convert(snapshot Snapshot) (int64, error) {
	numerator := new(big.Int).Mul(big.NewInt(snapshot.SourcePrincipalMinor), big.NewInt(snapshot.RateScaled))
	numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(snapshot.DestinationExponent)), nil))
	denominator := new(big.Int).Mul(big.NewInt(snapshot.RateScale), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(snapshot.SourceExponent)), nil))
	value := numerator.Quo(numerator, denominator)
	if !value.IsInt64() {
		return 0, errors.New("pricing snapshot conversion overflows")
	}
	return value.Int64(), nil
}

func validCurrency(currency string) bool {
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
