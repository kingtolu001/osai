package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/adapters/flutterwave"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
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
					service := reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
					if err = runAutomaticFlutterwave(service); err != nil {
						log.Fatal(err)
					}
					log.Println("reconciliation runtime ready")
					select {}
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
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return errors.New("settlement reconciliation boundary unavailable")
	}
	client := settlementv1.NewSettlementServiceClient(conn)
	rail := flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: key, Env: environment, Enabled: false})
	if !rail.Health().Capabilities["list_transactions"] {
		conn.Close()
		return errors.New("Flutterwave reconciliation rail unavailable")
	}
	go func() {
		defer conn.Close()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			passCtx, stop := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := reconcileFlutterwavePass(passCtx, service, client, rail, token, time.Now().UTC(), fee); err != nil {
				log.Printf("Flutterwave reconciliation pass failed: %v", err)
			}
			stop()
			<-ticker.C
		}
	}()
	return nil
}
