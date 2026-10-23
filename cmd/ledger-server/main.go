package main

import (
	"context"
	"database/sql"
	"log"
	"net"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgergrpc"
	"github.com/osai/osai/services/ledger/ledgerstore"
	"google.golang.org/grpc"
)

func main() {
	sandboxFees, policyErr := sandboxpolicy.Read()
	if policyErr != nil {
		log.Fatal(policyErr)
	}
	if err := observability.Init("ledger-server"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://osai:osai@localhost:5432/osai?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}
	if err := ledgerstore.EnsureSchema(db); err != nil {
		log.Fatal(err)
	}
	storage := ledgerstore.Postgres{DB: db, CustodyBankID: os.Getenv("OSAI_CUSTODY_BANK_ID")}
	sandboxToken := ""
	var accounting *ledgerstore.SandboxAccounting
	if sandboxFees {
		storage, err = storage.WithSandboxFees(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		sandboxToken = os.Getenv("OSAI_SANDBOX_LEDGER_TOKEN")
		accounting, err = ledgerstore.NewSandboxAccounting(context.Background(), db, "local-sandbox")
		if err != nil {
			log.Fatal(err)
		}
	}
	service, err := ledgerapi.NewServiceWithStorage(storage)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("OSAI_LEDGER_GRPC_ADDR")
	if addr == "" {
		addr = ":50051"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	server := grpc.NewServer()
	ledgerServer := &ledgergrpc.Server{Service: service, DB: db, LocalPrefundToken: os.Getenv("OSAI_LOCAL_PREFUND_TOKEN"), SandboxToken: sandboxToken}
	if accounting != nil {
		ledgerServer.SandboxAccounting = accounting
	}
	ledgerv1.RegisterLedgerServiceServer(server, ledgerServer)
	log.Printf("ledger gRPC server listening on %s", addr)
	log.Fatal(server.Serve(listener))
}
