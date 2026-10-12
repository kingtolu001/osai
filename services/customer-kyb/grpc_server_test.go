package main

import (
	"context"
	"net"
	"testing"

	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestCredentialLifecycleOverGRPC(t *testing.T) {
	service := NewService()
	institution, err := service.CreateInstitution("gRPC Credential Test", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	otherInstitution, err := service.CreateInstitution("Other gRPC Institution", "US", InstitutionStatusActive)
	if err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(server, &grpcServer{service: service})
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := customerv1.NewCustomerServiceClient(conn)

	created, err := client.CreateCredential(ctx, &customerv1.CreateCredentialRequest{
		InstitutionId: institution.ID,
		Label:         "pilot-production",
		Environment:   "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetSecret() == "" || created.GetCredential().GetEnvironment() != "production" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	credentialID := created.GetCredential().GetPublicId()
	authenticated, err := client.AuthenticateAPIClient(ctx, &customerv1.CredentialAuthRequest{
		PublicId: credentialID,
		Secret:   created.GetSecret(),
	})
	if err != nil || authenticated.GetEnvironment() != "production" {
		t.Fatalf("credential environment not returned by authentication: environment=%q err=%v", authenticated.GetEnvironment(), err)
	}
	if _, err := client.GetCredential(ctx, &customerv1.CredentialRequest{
		InstitutionId: otherInstitution.ID,
		PublicId:      credentialID,
	}); err == nil {
		t.Fatal("cross-institution credential read succeeded")
	}

	rotated, err := client.RotateCredential(ctx, &customerv1.CredentialRequest{
		InstitutionId: institution.ID,
		PublicId:      credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.GetSecret() == "" || rotated.GetSecret() == created.GetSecret() {
		t.Fatal("rotation did not return a distinct new secret")
	}
	if valid, _ := service.ValidateCredential(credentialID, created.GetSecret()); valid {
		t.Fatal("rotated credential still accepts old secret")
	}

	listed, err := client.ListCredentials(ctx, &customerv1.InstitutionRequest{InstitutionId: institution.ID})
	if err != nil || len(listed.GetCredentials()) != 1 {
		t.Fatalf("list credentials: count=%d err=%v", len(listed.GetCredentials()), err)
	}

	if _, err := client.RevokeCredential(ctx, &customerv1.CredentialRequest{
		InstitutionId: institution.ID,
		PublicId:      credentialID,
	}); err != nil {
		t.Fatal(err)
	}
	if valid, _ := service.ValidateCredential(credentialID, rotated.GetSecret()); valid {
		t.Fatal("revoked credential still accepts secret")
	}
}
