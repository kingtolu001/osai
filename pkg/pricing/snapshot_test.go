package pricing

import (
	"testing"
	"time"
)

func testSnapshot(mode string) Snapshot {
	snapshot := Snapshot{
		Mode: mode, RequestedAmountMinor: 10_000, SourcePrincipalMinor: 9_500,
		FeeMinor: 500, TotalDebitMinor: 10_000, DestinationAmountMinor: 9_500,
		SourceCurrency: "NGN", DestinationCurrency: "NGN", RateScaled: RateScale,
		RateDirection: "NGN_PER_NGN", RateScale: RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fee_v1",
		FeeScheduleVersion: 1, FixedFeeMinor: 500, MinimumFeeMinor: 500,
		MaximumFeeMinor: 500, MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}
	if mode == "FIXED_DESTINATION" {
		snapshot.RequestedAmountMinor = 9_500
		snapshot.DestinationTargetMinor = 9_500
	}
	hash, err := Hash(snapshot)
	if err != nil {
		panic(err)
	}
	snapshot.Hash = hash
	return snapshot
}

func TestValidateSnapshotBindsExactFeeAndDestinationArithmetic(t *testing.T) {
	for _, mode := range []string{"FIXED_SOURCE", "FIXED_DESTINATION"} {
		t.Run(mode, func(t *testing.T) {
			snapshot := testSnapshot(mode)
			if err := Validate(snapshot); err != nil {
				t.Fatalf("valid snapshot rejected: %v", err)
			}
			snapshot.FeeMinor++
			snapshot.Hash, _ = Hash(snapshot)
			if err := Validate(snapshot); err == nil {
				t.Fatal("snapshot with fee inconsistent with its schedule accepted")
			}
		})
	}
}

func TestValidateSnapshotRejectsTamperingAndNonMinimalFixedDestination(t *testing.T) {
	snapshot := testSnapshot("FIXED_SOURCE")
	snapshot.DestinationAmountMinor--
	snapshot.Hash, _ = Hash(snapshot)
	if err := Validate(snapshot); err == nil {
		t.Fatal("rate-inconsistent destination amount accepted")
	}

	snapshot = testSnapshot("FIXED_DESTINATION")
	snapshot.SourcePrincipalMinor++
	snapshot.TotalDebitMinor++
	snapshot.DestinationAmountMinor++
	snapshot.DestinationSurplusMinor++
	snapshot.Hash, _ = Hash(snapshot)
	if err := Validate(snapshot); err == nil {
		t.Fatal("non-minimal fixed-destination principal accepted")
	}
}
