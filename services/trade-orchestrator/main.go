package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/osai/osai/pkg/observability"
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
	store := tradecore.NewStore()
	grpcAddr := os.Getenv("OSAI_TRADE_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50054"
	}
	settlementAddr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if settlementAddr == "" {
		settlementAddr = "localhost:50055"
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
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("trade-orchestrator gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	tradev1.RegisterTradeServiceServer(server, newTradeServer(store, settlementClient))
	log.Printf("trade-orchestrator gRPC listening on %s", grpcAddr)
	if err := server.Serve(listener); err != nil {
		log.Fatalf("trade-orchestrator gRPC server failed: %v", err)
	}
}

func init() { fmt.Println("trade-orchestrator service initialized") }
