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
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"google.golang.org/grpc"
)

func main() {
	if err := observability.Init("customer-kyb"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	service := NewService()
	if err := seedSandboxCustomer(service); err != nil {
		log.Printf("sandbox seed warning: %v", err)
	}
	addr := os.Getenv("OSAI_KYB_PORT")
	if addr == "" { addr = ":8081" }
	grpcAddr := os.Getenv("OSAI_CUSTOMER_GRPC_ADDR")
	if grpcAddr == "" { grpcAddr = ":50052" }
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil { log.Fatalf("customer-kyb gRPC listen failed: %v", err) }
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
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
	if service == nil { return nil }
	instID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_INSTITUTION_ID"))
	if instID == "" { instID = "inst_sandbox_local" }
	clientID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_ID"))
	if clientID == "" { clientID = "ck_sandbox_local" }
	secret := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_SECRET"))
	if secret == "" { secret = "secret_local_001" }

	service.mu.Lock()
	if service.institutions[instID] == nil {
		service.institutions[instID] = &Institution{
			ID:        instID,
			Name:      "Sandbox Institution",
			Status:    InstitutionStatusActive,
			KYBStatus: KybStatusApproved,
			Country:   "US",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			APIEnabled: true,
		}
	}
	service.institutions[instID].KYBStatus = KybStatusApproved
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
	_ = os.Setenv("OSAI_SANDBOX_INSTITUTION_ID", instID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_ID", clientID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_SECRET", secret)
	return nil
}
