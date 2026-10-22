package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/services/settlement/ledgerclient"
	"github.com/osai/osai/services/settlement/settlementstore"
	"github.com/osai/osai/workflows"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
)

func main() {
	if _, err := sandboxpolicy.Read(); err != nil {
		log.Fatal(err)
	}
	if err := observability.Init("temporal-worker"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = observability.Shutdown(ctx)
	}()
	addr := os.Getenv("OSAI_TEMPORAL_ADDR")
	if addr == "" {
		addr = "localhost:7233"
	}
	temporalClient, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		log.Fatal(err)
	}
	defer temporalClient.Close()
	ledgerAddr := os.Getenv("OSAI_LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = "localhost:50051"
	}
	ledgerConnection, err := grpc.Dial(ledgerAddr, grpc.WithInsecure(), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()))
	if err != nil {
		log.Fatal(err)
	}
	defer ledgerConnection.Close()
	pair := firstpair.New()
	ledger := ledgerclient.New(ledgerConnection)
	activities := &workflows.ProviderSettlementActivities{Rail: pair.Settlement, Ledger: ledger, Funding: ledger}
	db, err := postgres.Open("settlement")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = settlementstore.EnsureSchema(db); err != nil {
		log.Fatal(err)
	}
	activities.Recorder = settlementstore.Postgres{DB: db}
	flutterwaveEnabled := os.Getenv("FLW_SETTLEMENT_ENABLED") == "true"
	if flutterwaveEnabled || os.Getenv("FLW_SECRET_KEY") != "" {
		if os.Getenv("FLW_SECRET_KEY") == "" || (flutterwaveEnabled && os.Getenv("FLW_SECRET_HASH") == "") || (os.Getenv("FLW_ENV") != "sandbox" && os.Getenv("FLW_ENV") != "test") {
			log.Fatal("Flutterwave sandbox settlement configuration unavailable")
		}
		if os.Getenv("FLW_ENV") == "sandbox" && !strings.HasPrefix(os.Getenv("FLW_SECRET_KEY"), "FLWSECK_TEST-") {
			log.Fatal("Flutterwave sandbox credential unavailable")
		}
		attempts := flutterwave.PostgresAttempts{DB: db}
		if err = attempts.EnsureSchema(); err != nil {
			log.Fatal(err)
		}
		activities.Rails = map[string]provider.SettlementRail{"flutterwave": flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: os.Getenv("FLW_SECRET_KEY"), SecretHash: os.Getenv("FLW_SECRET_HASH"), Env: os.Getenv("FLW_ENV"), Enabled: flutterwaveEnabled, Attempts: attempts})}
	}
	worker, err := workflows.StartSettlementWorker(nil, temporalClient, activities)
	if err != nil {
		log.Fatal(err)
	}
	defer worker.Stop()
	log.Println("Temporal settlement worker listening on task queue", workflows.TaskQueue)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}
