package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

func main() {
	if err := observability.Init("customer-kyb"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	db, err := postgres.Open("customer-kyb")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	service, err := NewPostgresService(db)
	if err != nil {
		log.Fatal(err)
	}
	if err := seedSandboxCustomer(service); err != nil {
		log.Printf("sandbox seed warning: %v", err)
	}
	addr := os.Getenv("OSAI_KYB_PORT")
	if addr == "" {
		addr = ":8081"
	}
	grpcAddr := os.Getenv("OSAI_CUSTOMER_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50052"
	}
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("customer-kyb gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	customerv1.RegisterCustomerServiceServer(server, &grpcServer{service: service})
	log.Printf("customer-kyb gRPC listening on %s", grpcAddr)
	go func() {
		httpServer := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"status":"ok","service":"customer-kyb"}`)
		}), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		log.Printf("customer-kyb health listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("customer-kyb health failed: %v", err)
		}
	}()
	if err := server.Serve(listener); err != nil {
		log.Fatalf("customer-kyb gRPC server failed: %v", err)
	}
	_ = service
}

func seedSandboxCustomer(service *Service) error {
	if service == nil {
		return nil
	}
	if os.Getenv("OSAI_SANDBOX_SEED") != "1" {
		return nil
	}
	instID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_INSTITUTION_ID"))
	if instID == "" {
		instID = "inst_sandbox_local"
	}
	clientID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_ID"))
	if clientID == "" {
		clientID = "ck_sandbox_local"
	}
	secret := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_SECRET"))
	if secret == "" {
		return fmt.Errorf("OSAI_SANDBOX_CLIENT_SECRET required when sandbox seed is enabled")
	}

	service.mu.Lock()
	if service.institutions[instID] == nil {
		service.institutions[instID] = &Institution{
			ID:         instID,
			Name:       "Institution A",
			Status:     InstitutionStatusActive,
			KYBStatus:  KybStatusApproved,
			Country:    "US",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			APIEnabled: true,
		}
	}
	service.institutions[instID].KYBStatus = KybStatusApproved
	service.institutions[instID].Name = "Institution A"
	service.institutions[instID].Status = InstitutionStatusActive
	service.institutions[instID].APIEnabled = true
	if service.publicToCred[clientID] == nil {
		cred := &Credential{
			ID:            "cred_sandbox_local",
			PublicID:      clientID,
			InstitutionID: instID,
			Status:        CredentialStatusActive,
			SecretHash:    hashSecret(secret, instID),
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}
		service.credentials[cred.ID] = cred
		service.publicToCred[cred.PublicID] = cred
	}
	service.mu.Unlock()
	if err := service.seedSandbox(*service.institutions[instID], *service.publicToCred[clientID]); err != nil {
		return err
	}
	_ = os.Setenv("OSAI_SANDBOX_INSTITUTION_ID", instID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_ID", clientID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_SECRET", secret)
	return nil
}
