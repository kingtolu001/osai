package main

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/observability"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

func main() {
	if err := observability.Init("customer-kyb"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	dsn := strings.TrimSpace(os.Getenv("OSAI_CUSTOMER_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	}
	if dsn == "" {
		log.Fatal("OSAI_CUSTOMER_POSTGRES_DSN or OSAI_POSTGRES_DSN is required")
	}
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open customer-kyb database: %v", err)
	}
	defer database.Close()
	if err = database.Ping(); err != nil {
		log.Fatalf("connect customer-kyb database: %v", err)
	}
	service, err := NewPostgresService(database)
	if err != nil {
		log.Fatalf("initialize customer-kyb persistence: %v", err)
	}
	if os.Getenv("OSAI_SANDBOX_SEED") == "1" {
		if err := seedSandboxCustomer(service); err != nil {
			log.Fatalf("seed sandbox customer: %v", err)
		}
	}
	operators, err := loadBeneficiaryOperators()
	if err != nil {
		log.Fatal(err)
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
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"status":"ok","service":"customer-kyb"}`)
		})
		mux.Handle("/v1/operator/beneficiaries/", service.beneficiaryOperatorHandler(operators))
		httpServer := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		log.Printf("customer-kyb health listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("customer-kyb health failed: %v", err)
		}
	}()
	if err := server.Serve(listener); err != nil {
		log.Fatalf("customer-kyb gRPC server failed: %v", err)
	}
}

func seedSandboxCustomer(service *Service) error {
	if service == nil {
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
		secret = "secret_local_001"
	}

	service.mu.Lock()
	inst, institutionExists := service.institutions[instID]
	if !institutionExists {
		entitlements, err := sandboxCustomerEntitlements()
		if err != nil {
			service.mu.Unlock()
			return err
		}
		inst = &Institution{
			ID:           instID,
			Name:         "Sandbox Institution",
			Status:       InstitutionStatusActive,
			KYBStatus:    KybStatusApproved,
			Country:      "US",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
			APIEnabled:   true,
			Entitlements: entitlements,
		}
		service.institutions[instID] = inst
	}
	cred, credentialExists := service.publicToCred[clientID]
	if !credentialExists {
		cred = &Credential{
			ID:            "cred_sandbox_local",
			PublicID:      clientID,
			InstitutionID: instID,
			Label:         "sandbox",
			Environment:   "sandbox",
			Status:        CredentialStatusActive,
			SecretHash:    hashSecret(secret, instID),
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}
		service.credentials[cred.ID] = cred
		service.publicToCred[cred.PublicID] = cred
	}
	if !institutionExists || !credentialExists {
		if err := service.seedSandbox(*inst, *cred); err != nil {
			service.mu.Unlock()
			return err
		}
	}
	webhookURL := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_WEBHOOK_URL"))
	if webhookURL == "" {
		webhookURL = "https://example.invalid/webhooks/customer/" + instID
	}
	webhookSecret := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_WEBHOOK_SECRET"))
	if webhookSecret == "" {
		webhookSecret = "wh_secret_" + instID
	}
	cfg := &CustomerWebhookConfig{
		InstitutionID: instID,
		WebhookURL:    webhookURL,
		SecretHash:    hashSecret(webhookSecret, instID),
		Enabled:       true,
		Status:        "ACTIVE",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := service.saveWebhookConfiguration(*cfg); err != nil {
		service.mu.Unlock()
		return err
	}
	service.webhookCfg[instID] = cfg
	service.mu.Unlock()
	_ = os.Setenv("OSAI_SANDBOX_INSTITUTION_ID", instID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_ID", clientID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_SECRET", secret)
	_ = os.Setenv("OSAI_SANDBOX_WEBHOOK_URL", webhookURL)
	_ = os.Setenv("OSAI_SANDBOX_WEBHOOK_SECRET", webhookSecret)
	return nil
}

func sandboxCustomerEntitlements() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_ENTITLEMENTS"))
	if raw == "" {
		return append([]string(nil), defaultInstitutionEntitlements...), nil
	}
	entitlements := make([]string, 0)
	for _, entitlement := range strings.Split(raw, ",") {
		entitlement = strings.TrimSpace(entitlement)
		if entitlement == "" {
			return nil, fmt.Errorf("OSAI_SANDBOX_ENTITLEMENTS contains an empty entitlement")
		}
		entitlements = append(entitlements, entitlement)
	}
	return entitlements, nil
}
