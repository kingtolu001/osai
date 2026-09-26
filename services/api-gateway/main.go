package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/osai/osai/pkg/observability"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := observability.Init("api-gateway"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	idempotency, err := newPersistentIdempotencyStore()
	if err != nil {
		log.Fatal(err)
	}
	defer idempotency.db.Close()
	gateway := &Gateway{idempotency: idempotency}
	customerAddr := os.Getenv("OSAI_CUSTOMER_GRPC_ADDR")
	if customerAddr == "" {
		customerAddr = "localhost:50052"
	}
	customerCtx, cancelCustomer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCustomer()
	customerConn, err := grpc.DialContext(customerCtx, customerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		log.Fatalf("dial customer service: %v", err)
	}
	defer customerConn.Close()
	gateway.SetCustomerClient(NewCustomerGRPCClient(customerConn))

	quoteAddr := os.Getenv("OSAI_QUOTE_GRPC_ADDR")
	if quoteAddr == "" {
		quoteAddr = "localhost:50053"
	}
	quoteCtx, cancelQuote := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelQuote()
	quoteConn, err := grpc.DialContext(quoteCtx, quoteAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		log.Fatalf("dial quote service: %v", err)
	}
	defer quoteConn.Close()
	gateway.SetQuoteService(NewQuoteGRPCClient(quoteConn))

	tradeAddr := os.Getenv("OSAI_TRADE_GRPC_ADDR")
	if tradeAddr == "" {
		tradeAddr = "localhost:50054"
	}
	tradeCtx, cancelTrade := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTrade()
	tradeConn, err := grpc.DialContext(tradeCtx, tradeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err == nil {
		defer tradeConn.Close()
		gateway.SetTradeClient(NewTradeGRPCClient(tradeConn))
	} else {
		log.Printf("dial trade service: %v", err)
	}

	settlementAddr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if settlementAddr == "" {
		settlementAddr = "localhost:50055"
	}
	settlementCtx, cancelSettlement := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSettlement()
	settlementConn, err := grpc.DialContext(settlementCtx, settlementAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err == nil {
		defer settlementConn.Close()
		gateway.SetSettlementClient(NewSettlementGRPCClient(settlementConn))
	} else {
		log.Printf("dial settlement service: %v", err)
	}
	reportingAddr := os.Getenv("OSAI_REPORTING_GRPC_ADDR")
	if reportingAddr == "" {
		log.Fatal("OSAI_REPORTING_GRPC_ADDR is required")
	}
	reportingCtx, cancelReporting := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReporting()
	reportingConn, err := grpc.DialContext(reportingCtx, reportingAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		log.Fatalf("dial reporting service: %v", err)
	}
	defer reportingConn.Close()
	gateway.SetReadModelClient(NewReportingGRPCClient(reportingConn))

	addr := os.Getenv("OSAI_API_GATEWAY_PORT")
	if addr == "" {
		addr = ":8082"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           gateway,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("api-gateway listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("api-gateway server failed: %v", err)
	}
}
