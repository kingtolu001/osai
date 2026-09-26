package main

import (
	"context"
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/provider"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type notificationServer struct {
	notificationv1.UnimplementedNotificationServiceServer
	store *webhookcore.Postgres
	token string
}

func (s *notificationServer) PublishEvent(ctx context.Context, event *notificationv1.Event) (*notificationv1.Receipt, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 || s.token == "" || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+s.token)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "service credential required")
	}
	if err := s.store.Publish(ctx, event); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &notificationv1.Receipt{EventId: event.EventId}, nil
}

func main() {
	if err := observability.Init("notification-webhook"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	db, err := postgres.Open("notification-webhook")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	secret := os.Getenv("OSAI_WEBHOOK_SECRET")
	token := os.Getenv("OSAI_NOTIFICATION_TOKEN")
	if secret == "" || token == "" {
		log.Fatal("OSAI_WEBHOOK_SECRET and OSAI_NOTIFICATION_TOKEN required")
	}
	store := &webhookcore.Postgres{DB: db, Client: &http.Client{Timeout: 5 * time.Second}, Secret: secret, MaxAttempts: 3}
	if raw := os.Getenv("OSAI_WEBHOOK_RETRY_MS"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 100 {
			log.Fatal("invalid OSAI_WEBHOOK_RETRY_MS")
		}
		store.RetryDelay = time.Duration(ms) * time.Millisecond
	}
	if err = store.EnsureSchema(); err != nil {
		log.Fatal(err)
	}
	if url := os.Getenv("OSAI_WEBHOOK_URL"); url != "" {
		if err = store.Configure("inst_sandbox_local", url, "env:OSAI_WEBHOOK_SECRET", []string{"quote.accepted"}); err != nil {
			log.Fatal(err)
		}
	}
	grpcAddr := os.Getenv("OSAI_NOTIFICATION_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50057"
	}
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatal(err)
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	notificationv1.RegisterNotificationServiceServer(grpcServer, &notificationServer{store: store, token: token})
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Printf("notification grpc: %v", err)
		}
	}()
	go func() {
		for range time.NewTicker(250 * time.Millisecond).C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			for i := 0; i < 20; i++ {
				done, err := store.DeliverDue(ctx)
				if err != nil {
					log.Printf("notification delivery: %v", err)
					break
				}
				if !done {
					break
				}
			}
			cancel()
		}
	}()
	port := os.Getenv("OSAI_WEBHOOK_PORT")
	if port == "" {
		port = ":8083"
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
	handler := webhookcore.NewHandler(map[string]provider.SettlementRail{"sim_lp_1": pair.Settlement}, webhookcore.TemporalSignaler{Client: temporalClient})
	server := &http.Server{Addr: port, Handler: handler}
	log.Printf("provider webhook ingress listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
