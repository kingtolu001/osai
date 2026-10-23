package ledgergrpc

import (
	"context"
	"encoding/json"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxrecon"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) GetSandboxPosition(ctx context.Context, req *ledgerv1.SandboxPositionRequest) (*ledgerv1.SandboxPositionResponse, error) {
	reader, ok := s.SandboxAccounting.(interface {
		Position(context.Context, string, string) (sandboxaccounting.Position, error)
	})
	if s.SandboxToken == "" || !ok {
		return nil, status.Error(codes.PermissionDenied, "sandbox position unavailable")
	}
	if err := s.authorizeSandbox(ctx); err != nil {
		return nil, err
	}
	if req == nil || req.InstitutionId == "" || req.TradeId == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox position identity required")
	}
	position, err := reader.Position(ctx, req.InstitutionId, req.TradeId)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "sandbox position unavailable")
	}
	raw, err := json.Marshal(position)
	if err != nil {
		return nil, status.Error(codes.Internal, "sandbox position unavailable")
	}
	response := &ledgerv1.SandboxPositionResponse{PositionJson: string(raw)}
	if req.Reconciliation {
		snapshots, ok := s.SandboxAccounting.(interface {
			ReconciliationSnapshot(context.Context, string, string) (sandboxrecon.Snapshot, error)
		})
		if !ok {
			return nil, status.Error(codes.PermissionDenied, "sandbox reconciliation unavailable")
		}
		snapshot, err := snapshots.ReconciliationSnapshot(ctx, req.InstitutionId, req.TradeId)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "sandbox reconciliation unavailable")
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return nil, status.Error(codes.Internal, "sandbox reconciliation unavailable")
		}
		response.ReconciliationJson = string(data)
	}
	return response, nil
}
