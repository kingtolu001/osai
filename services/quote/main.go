package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/observability"
	"google.golang.org/grpc/credentials/insecure"

	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc"
)

func main() {
	if err := observability.Init("quote"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	store := quotecore.NewQuoteStore()
	httpPort := os.Getenv("OSAI_QUOTE_PORT")
	if httpPort == "" { httpPort = ":8084" }
	grpcPort := os.Getenv("OSAI_QUOTE_GRPC_ADDR")
	if grpcPort == "" { grpcPort = ":50053" }
	listener, err := net.Listen("tcp", grpcPort)
	if err != nil { log.Fatalf("quote gRPC listen failed: %v", err) }
	tradeAddr := os.Getenv("OSAI_TRADE_GRPC_ADDR")
	if tradeAddr == "" { tradeAddr = "localhost:50054" }
	tradeConn, err := grpc.Dial(tradeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
	if err != nil { log.Fatalf("dial trade-orchestrator: %v", err) }
	defer tradeConn.Close()
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	quotev1.RegisterQuoteServiceServer(server, &grpcServer{store: store, tradeClient: tradev1.NewTradeServiceClient(tradeConn)})
	log.Printf("quote gRPC listening on %s", grpcPort)
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "quote"})
		})
		http.HandleFunc("/v1/quotes", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				CustomerID      string `json:"customer_id"`
				BaseAmountMinor int64  `json:"base_amount_minor"`
				BaseCurrency    string `json:"base_currency"`
				QuoteCurrency   string `json:"quote_currency"`
				DestinationRail string `json:"destination_rail"`
				Urgency         string `json:"urgency"`
				IdempotencyKey  string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "VALIDATION_ERROR", "message": "malformed JSON"}})
				return
			}
			if req.CustomerID == "" { req.CustomerID = "inst_sandbox_local" }
			if req.IdempotencyKey == "" { req.IdempotencyKey = "quote-key-" + time.Now().UTC().Format(time.RFC3339Nano) }
			quote, err := store.Create(quotecore.QuoteRequest{CustomerID: req.CustomerID, BaseAmountMinor: req.BaseAmountMinor, BaseCurrency: req.BaseCurrency, QuoteCurrency: req.QuoteCurrency, DestinationRail: req.DestinationRail, Urgency: req.Urgency, IdempotencyKey: req.IdempotencyKey}, req.DestinationRail, 1200, 500, req.BaseAmountMinor/100, time.Now().Add(30*time.Minute))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "VALIDATION_ERROR", "message": err.Error()}})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"quote_id": quote.ID, "status": string(quote.Status), "base_amount_minor": quote.Request.BaseAmountMinor, "base_currency": quote.Request.BaseCurrency, "quote_currency": quote.Request.QuoteCurrency, "amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor, "expires_at": quote.ExpiresAt.UTC().Format(time.RFC3339)})
		})
		http.HandleFunc("/v1/quotes/", func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/v1/quotes/")
			if path == "" || strings.Contains(path, "/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			quote, ok := store.Get(path)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"quote_id": quote.ID, "status": string(quote.Status), "base_amount_minor": quote.Request.BaseAmountMinor, "base_currency": quote.Request.BaseCurrency, "quote_currency": quote.Request.QuoteCurrency, "amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor, "expires_at": quote.ExpiresAt.UTC().Format(time.RFC3339)})
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
