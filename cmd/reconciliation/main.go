package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/observability"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if os.Getenv("OSAI_SANDBOX_RECONCILIATION_URL") != "" || os.Getenv("OSAI_SANDBOX_RECONCILIATION_KEY") != "" || os.Getenv("OSAI_SANDBOX_RECONCILIATION_INSTITUTION") != "" || os.Getenv("OSAI_SANDBOX_RECONCILIATION_TRADE_ID") != "" {
		if err := runSandboxReconciliation(); err != nil {
			log.Print("sandbox reconciliation unavailable or unresolved; inspect durable breaks")
			os.Exit(1)
		}
		return
	}
	if err := observability.Init("reconciliation"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = observability.Shutdown(ctx)
	}()
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://osai:osai@localhost:5432/osai?sslmode=disable"
	}
	log.Println("reconciliation runtime starting")
	for i := 0; i < 10; i++ {
		db, err := sql.Open("pgx", dsn)
		if err == nil {
			if err = db.Ping(); err == nil {
				if err = reconciliation.EnsureSchema(db); err == nil {
					service, loadErr := reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
					if loadErr != nil {
						log.Fatal("reconciliation durable state unavailable")
					}
					if err = runAutomaticFlutterwave(service); err != nil {
						log.Fatal(err)
					}
					log.Println("reconciliation runtime ready")
					ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
					defer stop()
					<-ctx.Done()
					return
				}
			}
			db.Close()
		}
		log.Printf("waiting for postgres: attempt %d/10: %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	log.Fatal("reconciliation runtime failed to connect to postgres")
}

func runAutomaticFlutterwave(service *reconciliation.Service) error {
	if os.Getenv("FLW_RECONCILIATION_ENABLED") != "true" {
		return nil
	}
	key := os.Getenv("FLW_SECRET_KEY")
	environment := os.Getenv("FLW_ENV")
	token := os.Getenv("OSAI_RECONCILIATION_TOKEN")
	fee, err := strconv.ParseInt(os.Getenv("OSAI_FLW_EXPECTED_FEE_MINOR"), 10, 64)
	if key == "" || token == "" || fee < 0 || err != nil || (environment != "test" && environment != "sandbox") || (environment == "sandbox" && !strings.HasPrefix(key, "FLWSECK_TEST-")) {
		return errors.New("automatic Flutterwave reconciliation configuration unavailable")
	}
	addr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if addr == "" {
		addr = "localhost:50055"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor()), grpc.WithBlock())
	if err != nil {
		return errors.New("settlement reconciliation boundary unavailable")
	}
	client := settlementv1.NewSettlementServiceClient(conn)
	rail := flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: key, Env: environment, Enabled: false})
	if !rail.Health().Capabilities["list_transactions"] {
		conn.Close()
		return errors.New("Flutterwave reconciliation rail unavailable")
	}
	lag, interval := 5*time.Minute, time.Minute
	if raw := os.Getenv("OSAI_LOCAL_RECONCILIATION_LAG"); raw != "" {
		base, parseErr := url.Parse(os.Getenv("FLW_BASE_URL"))
		if environment != "test" || parseErr != nil || base == nil || base.Scheme != "http" || (base.Hostname() != "127.0.0.1" && base.Hostname() != "localhost") {
			conn.Close()
			return errors.New("local reconciliation timing requires a loopback test provider")
		}
		lag, err = time.ParseDuration(raw)
		if err != nil || lag < 0 || lag > 5*time.Minute {
			conn.Close()
			return errors.New("invalid local reconciliation lag")
		}
		interval = time.Second
	}
	go func() {
		defer conn.Close()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			passCtx, stop := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := reconcileFlutterwavePassWithLag(passCtx, service, client, rail, token, time.Now().UTC(), fee, lag); err != nil {
				log.Printf("Flutterwave reconciliation pass failed: %v", err)
			}
			stop()
			<-ticker.C
		}
	}()
	return nil
}
