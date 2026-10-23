package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/observability"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := observability.Init("quote"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	dsn := strings.TrimSpace(os.Getenv("OSAI_QUOTE_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	}
	if dsn == "" {
		log.Fatal("OSAI_QUOTE_POSTGRES_DSN or OSAI_POSTGRES_DSN is required")
	}
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open quote database: %v", err)
	}
	defer database.Close()
	if err = database.Ping(); err != nil {
		log.Fatalf("connect quote database: %v", err)
	}
	store, err := quotecore.NewPostgresStore(database)
	if err != nil {
		log.Fatalf("initialize quote persistence: %v", err)
	}
	token := strings.TrimSpace(os.Getenv("OSAI_NOTIFICATION_TOKEN"))
	if token == "" {
		log.Fatal("OSAI_NOTIFICATION_TOKEN required")
	}
	notificationAddr := os.Getenv("OSAI_NOTIFICATION_GRPC_ADDR")
	if notificationAddr == "" {
		notificationAddr = "localhost:50057"
	}
	notificationConn, err := grpc.Dial(notificationAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial notification service: %v", err)
	}
	defer notificationConn.Close()
	notificationClient := notificationv1.NewNotificationServiceClient(notificationConn)
	liquidity := firstpair.New().Liquidity
	go func() {
		for range time.NewTicker(250 * time.Millisecond).C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := store.ExpireDue(ctx, time.Now().UTC()); err != nil {
				log.Printf("quote expiry worker: %v", err)
			}
			if err := store.Relay(ctx, notificationClient, token); err != nil {
				log.Printf("quote outbox relay: %v", err)
			}
			cancel()
		}
	}()
	httpPort := os.Getenv("OSAI_QUOTE_PORT")
	if httpPort == "" {
		httpPort = ":8084"
	}
	grpcPort := os.Getenv("OSAI_QUOTE_GRPC_ADDR")
	if grpcPort == "" {
		grpcPort = ":50053"
	}
	listener, err := net.Listen("tcp", grpcPort)
	if err != nil {
		log.Fatalf("quote gRPC listen failed: %v", err)
	}
	tradeAddr := os.Getenv("OSAI_TRADE_GRPC_ADDR")
	if tradeAddr == "" {
		tradeAddr = "localhost:50054"
	}
	tradeConn, err := grpc.Dial(tradeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		log.Fatalf("dial trade-orchestrator: %v", err)
	}
	defer tradeConn.Close()
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	quoteService := &grpcServer{store: store, pricing: store, tradeClient: tradev1.NewTradeServiceClient(tradeConn), liquidity: liquidity}
	quotev1.RegisterQuoteServiceServer(server, quoteService)
	startQuoteAcceptanceRecovery(context.Background(), quoteService)
	pricingOperators, err := loadPricingOperators()
	if err != nil {
		log.Fatal(err)
	}
	if len(pricingOperators) == 0 {
		log.Printf("quote pricing operator listener disabled: no operator identities configured")
	} else {
		adminAddress := strings.TrimSpace(os.Getenv("OSAI_QUOTE_PRICING_ADMIN_ADDR"))
		if adminAddress == "" {
			adminAddress = "127.0.0.1:8085"
		}
		adminCertificate := strings.TrimSpace(os.Getenv("OSAI_QUOTE_PRICING_ADMIN_TLS_CERT"))
		adminKey := strings.TrimSpace(os.Getenv("OSAI_QUOTE_PRICING_ADMIN_TLS_KEY"))
		go func() {
			if err := servePricingAdmin(adminAddress, adminCertificate, adminKey, quoteService.pricingOperatorHandler(pricingOperators)); err != nil && err != http.ErrServerClosed {
				log.Printf("quote pricing admin listener failed: %v", err)
			}
		}()
	}
	log.Printf("quote gRPC listening on %s", grpcPort)
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "quote"})
		})
		log.Printf("quote health listening on %s", httpPort)
		if err := http.ListenAndServe(httpPort, mux); err != nil {
			log.Printf("quote http server failed: %v", err)
		}
	}()
	if err := server.Serve(listener); err != nil {
		log.Fatalf("quote gRPC server failed: %v", err)
	}
}

func init() { fmt.Println("quote service initialized") }
