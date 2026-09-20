package main

import (
	"database/sql"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	reconciliation "github.com/osai/osai/services/reconciliation"
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
					_ = service
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
