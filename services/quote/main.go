package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
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
	db, err := postgres.Open("quote")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	store, err := quotecore.NewPostgresStore(db)
	if err != nil {
		log.Fatal(err)
	}
	token := os.Getenv("OSAI_NOTIFICATION_TOKEN")
	if token == "" {
		log.Fatal("OSAI_NOTIFICATION_TOKEN required")
	}
	notificationAddr := os.Getenv("OSAI_NOTIFICATION_GRPC_ADDR")
	if notificationAddr == "" {
		notificationAddr = "localhost:50057"
	}
	notificationConn, err := grpc.Dial(notificationAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer notificationConn.Close()
	notificationClient := notificationv1.NewNotificationServiceClient(notificationConn)
	go func() {
		for range time.NewTicker(250 * time.Millisecond).C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	quotev1.RegisterQuoteServiceServer(server, &grpcServer{store: store, tradeClient: tradev1.NewTradeServiceClient(tradeConn), liquidity: firstpair.New().Liquidity})
	log.Printf("quote gRPC listening on %s", grpcPort)
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "quote"})
		})
		log.Printf("quote health listening on %s", httpPort)
		if err := http.ListenAndServe(httpPort, nil); err != nil {
			log.Printf("quote http server failed: %v", err)
		}
	}()
	if err := server.Serve(listener); err != nil {
		log.Fatalf("quote gRPC server failed: %v", err)
	}
}

func init() { fmt.Println("quote service initialized") }
