package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/observability"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := observability.Init("trade-orchestrator"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	dsn := strings.TrimSpace(os.Getenv("OSAI_TRADE_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	}
	if dsn == "" {
		log.Fatal("OSAI_TRADE_POSTGRES_DSN or OSAI_POSTGRES_DSN is required")
	}
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open trade database: %v", err)
	}
	defer database.Close()
	if err = database.Ping(); err != nil {
		log.Fatalf("connect trade database: %v", err)
	}
	store, err := tradecore.NewPostgresStore(database)
	if err != nil {
		log.Fatalf("initialize trade persistence: %v", err)
	}
	grpcAddr := os.Getenv("OSAI_TRADE_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50054"
	}
	settlementAddr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if settlementAddr == "" {
		settlementAddr = "localhost:50055"
	}
	customerAddr := os.Getenv("OSAI_CUSTOMER_GRPC_ADDR")
	if customerAddr == "" {
		customerAddr = "localhost:50052"
	}
	ledgerAddr := os.Getenv("OSAI_LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = "localhost:50051"
	}
	var settlementClient settlementv1.SettlementServiceClient
	if strings.TrimSpace(settlementAddr) != "" {
		conn, err := grpc.Dial(settlementAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
		if err != nil {
			log.Printf("trade-orchestrator could not dial settlement service: %v", err)
		} else {
			defer conn.Close()
			settlementClient = settlementv1.NewSettlementServiceClient(conn)
		}
	}
	tradeFinalityToken := strings.TrimSpace(os.Getenv("OSAI_TRADE_SETTLEMENT_TOKEN"))
	if tradeFinalityToken == "" {
		log.Fatal("OSAI_TRADE_SETTLEMENT_TOKEN is required")
	}
	if settlementClient == nil {
		log.Fatal("settlement service is required for durable trade finality")
	}
	go runSettlementFinalityConsumer(context.Background(), settlementClient, store, tradeFinalityToken)
	var customerClient customerv1.CustomerServiceClient
	if strings.TrimSpace(customerAddr) != "" {
		conn, err := grpc.Dial(customerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
		if err != nil {
			log.Printf("trade-orchestrator could not dial customer service: %v", err)
		} else {
			defer conn.Close()
			customerClient = customerv1.NewCustomerServiceClient(conn)
		}
	}
	var ledgerClient ledgerv1.LedgerServiceClient
	if strings.TrimSpace(ledgerAddr) != "" {
		conn, err := grpc.Dial(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
		if err != nil {
			log.Printf("trade-orchestrator could not dial ledger service: %v", err)
		} else {
			defer conn.Close()
			ledgerClient = ledgerv1.NewLedgerServiceClient(conn)
		}
	}
	notificationToken := strings.TrimSpace(os.Getenv("OSAI_NOTIFICATION_TOKEN"))
	if notificationToken == "" {
		log.Fatal("OSAI_NOTIFICATION_TOKEN is required for durable customer events")
	}
	notificationAddr := strings.TrimSpace(os.Getenv("OSAI_NOTIFICATION_GRPC_ADDR"))
	if notificationAddr == "" {
		notificationAddr = "localhost:50057"
	}
	notificationConn, err := grpc.Dial(notificationAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
	if err != nil {
		log.Fatalf("dial notification service: %v", err)
	}
	defer notificationConn.Close()
	notificationClient := notificationv1.NewNotificationServiceClient(notificationConn)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := store.Relay(ctx, notificationClient, notificationToken); err != nil {
				log.Printf("trade outbox relay: %v", err)
			}
			cancel()
		}
	}()
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("trade-orchestrator gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	tradev1.RegisterTradeServiceServer(server, newTradeServer(store, settlementClient).withCustomerClient(customerClient).withLedgerClient(ledgerClient))
	log.Printf("trade-orchestrator gRPC listening on %s", grpcAddr)
	if err := server.Serve(listener); err != nil {
		log.Fatalf("trade-orchestrator gRPC server failed: %v", err)
	}
}

func init() { fmt.Println("trade-orchestrator service initialized") }

func runSettlementFinalityConsumer(ctx context.Context, settlement settlementv1.SettlementServiceClient, store *tradecore.Store, token string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := consumeSettlementFinality(callCtx, settlement, store, token); err != nil {
			log.Printf("settlement finality consumer: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
