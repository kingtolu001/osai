package pricing

import (
	"errors"
	"math/big"
)

var ErrInvalidQuoteAmount = errors.New("invalid quote amount")
var ErrPricingOverflow = errors.New("quote amount exceeds supported integer range")

// ConvertMinorUnits performs exact fixed-point conversion with floor rounding.
func ConvertMinorUnits(sourceMinor, rateScaled int64, sourceExponent, destinationExponent uint8) (int64, error) {
	if sourceMinor < 0 || rateScaled <= 0 || sourceExponent > maxCurrencyExponent || destinationExponent > maxCurrencyExponent {
		return 0, ErrInvalidQuoteAmount
	}
	numerator := new(big.Int).Mul(big.NewInt(sourceMinor), big.NewInt(rateScaled))
	numerator.Mul(numerator, pow10(destinationExponent))
	denominator := new(big.Int).Mul(pow10(sourceExponent), big.NewInt(RateScale))
	result := numerator.Quo(numerator, denominator)
	if !result.IsInt64() {
		return 0, ErrPricingOverflow
	}
	return result.Int64(), nil
}

func pow10(exponent uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}
