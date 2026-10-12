package quotecore

import (
	"errors"
	"math"
	"testing"
)

func testFeeSchedule() FeeSchedule {
	return FeeSchedule{
		ID:                          "fee-ngn-usd-v1",
		Version:                     1,
		InstitutionID:               "inst_test",
		SourceCurrency:              "NGN",
		DestinationCurrency:         "USD",
		SourceExponent:              2,
		DestinationExponent:         2,
		MinimumSourcePrincipalMinor: 1,
		MaximumSourcePrincipalMinor: 10_000_000_000,
		MinimumTotalDebitMinor:      1,
		MaximumTotalDebitMinor:      20_000_000_000,
		MinimumDestinationMinor:     1,
		MaximumDestinationMinor:     30_000_000_000,
		FixedFeeMinor:               10,
		FeeBasisPoints:              100,
		MinimumFeeMinor:             25,
		MaximumFeeMinor:             1_000_000,
	}
}

func TestPriceFixedSourceCalculatesAllInDebit(t *testing.T) {
	got, err := PriceQuote(testFeeSchedule(), QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 10_110,
		RateScaled:        RateScale,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourcePrincipalMinor != 10_000 || got.FeeMinor != 110 || got.TotalDebitMinor != 10_110 || got.DestinationMinor != 10_000 {
		t.Fatalf("unexpected fixed-source result: %+v", got)
	}
}

func TestPriceFixedSourceRejectsUnrepresentableDebit(t *testing.T) {
	_, err := PriceQuote(testFeeSchedule(), QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 10_111,
		RateScaled:        RateScale,
	})
	if !errors.Is(err, ErrUnrepresentableDebit) {
		t.Fatalf("expected unrepresentable debit, got %v", err)
	}
}

func TestPriceFixedDestinationUsesConservativeRoundingAndTracksSurplus(t *testing.T) {
	got, err := PriceQuote(testFeeSchedule(), QuotePricingInput{
		Mode:                   QuoteModeFixedDestination,
		DestinationAmountMinor: 15_000_001,
		RateScaled:             150_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourcePrincipalMinor != 10_001 ||
		got.DestinationMinor != 15_001_500 ||
		got.DestinationSurplusMinor != 1_499 ||
		got.TotalDebitMinor != got.SourcePrincipalMinor+got.FeeMinor {
		t.Fatalf("unexpected fixed-destination result: %+v", got)
	}
	previousProceeds, err := ConvertMinorUnits(got.SourcePrincipalMinor-1, 150_000_000_000, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if previousProceeds >= 15_000_001 {
		t.Fatalf("fixed-destination principal was not minimal: principal=%d previous_proceeds=%d", got.SourcePrincipalMinor, previousProceeds)
	}
}

func TestPriceQuoteUsesCurrencyExponents(t *testing.T) {
	policy := testFeeSchedule()
	policy.SourceCurrency = "JPY"
	policy.DestinationCurrency = "KWD"
	policy.SourceExponent = 0
	policy.DestinationExponent = 3
	policy.MaximumSourcePrincipalMinor = 1_000_000
	policy.FeeBasisPoints = 0
	policy.FixedFeeMinor = 0
	policy.MinimumFeeMinor = 0
	policy.MaximumFeeMinor = 0

	got, err := PriceQuote(policy, QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 3,
		RateScaled:        50_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourcePrincipalMinor != 3 || got.DestinationMinor != 1_500 {
		t.Fatalf("currency exponent conversion failed: %+v", got)
	}
}

func TestPriceQuoteAppliesFeeMinimumAndMaximum(t *testing.T) {
	policy := testFeeSchedule()
	policy.FeeBasisPoints = 0
	policy.FixedFeeMinor = 1
	policy.MinimumFeeMinor = 25
	policy.MaximumFeeMinor = 25
	got, err := PriceQuote(policy, QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 1_025,
		RateScaled:        RateScale,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.FeeMinor != 25 || got.SourcePrincipalMinor != 1_000 {
		t.Fatalf("fee bounds not applied: %+v", got)
	}
}

func TestPercentageFeeRoundsUpAndConversionRoundsDown(t *testing.T) {
	policy := testFeeSchedule()
	policy.FixedFeeMinor = 2
	policy.FeeBasisPoints = 1
	policy.MinimumFeeMinor = 0
	policy.MaximumFeeMinor = 0

	got, err := PriceQuote(policy, QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 4,
		RateScaled:        RateScale,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourcePrincipalMinor != 1 || got.FeeMinor != 3 || got.TotalDebitMinor != 4 {
		t.Fatalf("percentage fee must round up before adding fixed fee: %+v", got)
	}

	proceeds, err := ConvertMinorUnits(1_499, 66_667, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if proceeds != 0 {
		t.Fatalf("positive conversion must floor minor units; got %d", proceeds)
	}
}

func TestPriceQuoteRejectsInvalidModeInputsAndPairRate(t *testing.T) {
	policy := testFeeSchedule()
	cases := []QuotePricingInput{
		{Mode: QuoteModeFixedSource, SourceAmountMinor: 100, DestinationAmountMinor: 1, RateScaled: RateScale},
		{Mode: QuoteModeFixedDestination, DestinationAmountMinor: 100, SourceAmountMinor: 1, RateScaled: RateScale},
		{Mode: QuoteModeFixedSource, SourceAmountMinor: 100, RateScaled: 0},
		{Mode: "UNKNOWN", RateScaled: RateScale},
	}
	for _, input := range cases {
		if _, err := PriceQuote(policy, input); !errors.Is(err, ErrInvalidQuoteAmount) && !errors.Is(err, ErrUnrepresentableDebit) {
			t.Errorf("expected invalid quote input for %+v, got %v", input, err)
		}
	}
	policy.DestinationCurrency = policy.SourceCurrency
	if _, err := PriceQuote(policy, QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: 100,
		RateScaled:        2 * RateScale,
	}); !errors.Is(err, ErrInvalidQuoteAmount) {
		t.Fatalf("same-currency quote must use parity, got %v", err)
	}
}

func TestPriceQuoteRejectsOverflow(t *testing.T) {
	policy := testFeeSchedule()
	policy.FeeBasisPoints = 0
	policy.FixedFeeMinor = 0
	policy.MinimumFeeMinor = 0
	policy.MaximumFeeMinor = 0
	policy.MaximumSourcePrincipalMinor = math.MaxInt64
	policy.MaximumTotalDebitMinor = math.MaxInt64
	_, err := PriceQuote(policy, QuotePricingInput{
		Mode:              QuoteModeFixedSource,
		SourceAmountMinor: math.MaxInt64,
		RateScaled:        math.MaxInt64,
	})
	if !errors.Is(err, ErrPricingOverflow) && !errors.Is(err, ErrUnrepresentableDebit) {
		t.Fatalf("expected overflow or non-representable debit, got %v", err)
	}
}

func TestFeeScheduleValidatesLimitsAndCurrencyCodes(t *testing.T) {
	policy := testFeeSchedule()
	policy.SourceExponent = maxCurrencyExponent + 1
	if err := policy.Validate(); !errors.Is(err, ErrInvalidPricingPolicy) {
		t.Fatalf("expected exponent validation, got %v", err)
	}
	policy = testFeeSchedule()
	policy.MaximumFeeMinor = policy.MinimumFeeMinor - 1
	if err := policy.Validate(); !errors.Is(err, ErrInvalidPricingPolicy) {
		t.Fatalf("expected fee limit validation, got %v", err)
	}
	policy = testFeeSchedule()
	policy.SourceCurrency = "US"
	if err := policy.Validate(); !errors.Is(err, ErrInvalidPricingPolicy) {
		t.Fatalf("expected currency validation, got %v", err)
	}
}
