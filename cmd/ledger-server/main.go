package main

import (
	"database/sql"
	"log"
	"net"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgergrpc"
	"github.com/osai/osai/services/ledger/ledgerstore"
	"google.golang.org/grpc"
)

func main() {
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
	service, err := ledgerapi.NewServiceWithStorage(ledgerstore.Postgres{DB: db})
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal(err)
	}
	server := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(server, &ledgergrpc.Server{Service: service})
	log.Println("ledger gRPC server listening on :50051")
	log.Fatal(server.Serve(listener))
}
