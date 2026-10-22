package main

import (
	"context"
	"testing"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/provider"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

func TestWebhookDeliveryHistoryRequiresAdminAuthorization(t *testing.T) {
	server := &notificationGRPCServer{configureToken: "admin-token", publishToken: "publish-token"}
	request := &notificationv1.GetWebhookDeliveryHistoryRequest{InstitutionId: "inst_test", EventId: "evt_test", Limit: 10}

	for name, token := range map[string]string{"missing": "", "publisher": "publish-token", "wrong": "other-token"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if token != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
			}
			if _, err := server.GetWebhookDeliveryHistory(ctx, request); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("history access code=%s, want PermissionDenied", status.Code(err))
			}
			if _, err := server.ListWebhookDeliveryEvents(ctx, &notificationv1.ListWebhookDeliveryEventsRequest{InstitutionId: "inst_test", Limit: 10}); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("delivery operations access code=%s, want PermissionDenied", status.Code(err))
			}
		})
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer admin-token"))
	if _, err := server.GetWebhookDeliveryHistory(ctx, request); status.Code(err) != codes.Unavailable {
		t.Fatalf("authorized history request without persistence code=%s, want Unavailable", status.Code(err))
	}
	if _, err := server.ListWebhookDeliveryEvents(ctx, &notificationv1.ListWebhookDeliveryEventsRequest{InstitutionId: "inst_test", Limit: 10}); status.Code(err) != codes.Unavailable {
		t.Fatalf("authorized delivery operations without persistence code=%s, want Unavailable", status.Code(err))
	}
}
