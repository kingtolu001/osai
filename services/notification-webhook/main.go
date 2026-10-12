package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/provider"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
)

func main() {
	if err := observability.Init("notification-webhook"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	database, err := postgres.Open("notification-webhook")
	if err != nil {
		log.Fatalf("connect notification-webhook database: %v", err)
	}
	defer database.Close()
	inbox := webhookcore.PostgresProviderInbox{DB: database}
	if err := inbox.EnsureSchema(); err != nil {
		log.Fatalf("initialize provider webhook inbox: %v", err)
	}
	store := &webhookcore.Postgres{
		DB: database, Client: &http.Client{Timeout: 10 * time.Second},
		ConfigEncryptionKey: []byte(strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_CONFIG_ENCRYPTION_KEY"))),
		MaxAttempts:         5,
	}
	if len(store.ConfigEncryptionKey) < 32 {
		log.Fatal("OSAI_WEBHOOK_CONFIG_ENCRYPTION_KEY must contain at least 32 bytes")
	}
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("initialize customer webhook persistence: %v", err)
	}
	publishToken := strings.TrimSpace(os.Getenv("OSAI_NOTIFICATION_TOKEN"))
	configureToken := strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_ADMIN_TOKEN"))
	if publishToken == "" || configureToken == "" {
		log.Fatal("OSAI_NOTIFICATION_TOKEN and OSAI_WEBHOOK_ADMIN_TOKEN are required")
	}
	if raw := strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_MAX_ATTEMPTS")); raw != "" {
		max, err := strconv.Atoi(raw)
		if err != nil || max < 1 || max > 20 {
			log.Fatal("OSAI_WEBHOOK_MAX_ATTEMPTS must be between 1 and 20")
		}
		store.MaxAttempts = max
	}
	if raw := strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_RETRY_DELAY")); raw != "" {
		delay, err := time.ParseDuration(raw)
		if err != nil || delay <= 0 {
			log.Fatal("OSAI_WEBHOOK_RETRY_DELAY must be a positive duration")
		}
		store.RetryDelay = delay
	}
	temporalAddr := strings.TrimSpace(os.Getenv("OSAI_TEMPORAL_ADDR"))
	if temporalAddr == "" {
		temporalAddr = "localhost:7233"
	}
	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddr})
	if err != nil {
		log.Fatal(err)
	}
	defer temporalClient.Close()
	pair := firstpair.New()
	handler := webhookcore.NewHandler(providerRails(pair.Settlement), webhookcore.TemporalSignaler{Client: temporalClient})
	handler.SetProviderInbox(inbox)
	httpPort := os.Getenv("OSAI_WEBHOOK_PORT")
	if httpPort == "" {
		httpPort = ":8083"
	}
	httpServer := &http.Server{Addr: httpPort, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("provider webhook ingress listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("provider webhook ingress failed: %v", err)
		}
	}()
	go runCustomerWebhookDelivery(context.Background(), store)
	grpcAddr := os.Getenv("OSAI_NOTIFICATION_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50057"
	}
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("notification gRPC listen failed: %v", err)
	}
	server := grpc.NewServer()
	notificationv1.RegisterNotificationServiceServer(server, &notificationGRPCServer{store: store, publishToken: publishToken, configureToken: configureToken})
	log.Printf("notification gRPC listening on %s", grpcAddr)
	if err := server.Serve(listener); err != nil {
		log.Fatal(err)
	}
}

func runCustomerWebhookDelivery(ctx context.Context, store *webhookcore.Postgres) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		for {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			delivered, err := store.DeliverDue(callCtx)
			cancel()
			if err != nil {
				log.Printf("customer webhook delivery: %v", err)
				break
			}
			if !delivered {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func providerRails(simulator provider.SettlementRail) map[string]provider.SettlementRail {
	return map[string]provider.SettlementRail{
		"sim_lp_1":    simulator,
		"flutterwave": flutterwave.New(flutterwave.Config{SecretHash: os.Getenv("FLW_SECRET_HASH")}),
	}
}
