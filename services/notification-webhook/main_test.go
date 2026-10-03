package main

import (
	"testing"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/provider"
)

func TestProviderRailsRegistersFlutterwave(t *testing.T) {
	t.Setenv("FLW_SECRET_HASH", "local-test-hash")
	rails := providerRails(firstpair.New().Settlement)
	if rails["sim_lp_1"] == nil || rails["flutterwave"] == nil || !rails["flutterwave"].Health().Capabilities["webhook"] {
		t.Fatal("provider webhook registration incomplete")
	}
	if _, err := rails["flutterwave"].VerifyWebhook(nil, map[string]string{"Verif-Hash": "wrong"}); err == nil {
		t.Fatal("registered Flutterwave rail accepted wrong verification")
	}
	var _ provider.SettlementRail = rails["flutterwave"]
}
