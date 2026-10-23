package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/pkg/sandboxrecon"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgergrpc"
	"github.com/osai/osai/services/ledger/ledgerstore"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSandboxIndependentStatementReconciliation(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof PostgreSQL required")
	}
	if !sandboxpolicy.LocalProofDSN(dsn) {
		t.Fatal("local proof database required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	accounting, err := ledgerstore.NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err = reconciliation.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	service, err := reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	institution, trade := "sbx:statement:"+id, "trd_"+id
	terms := sandboxaccounting.Terms{InstitutionID: institution, TradeID: trade, PrincipalMinor: 9500, FeeMinor: 500, Plan: &sandboxaccounting.FXPlan{QuoteID: "sbx:quote:" + id, Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 95, RateDirection: fxevidence.DestinationPerSource, BeneficiaryID: "sbx:beneficiary", ProviderID: "sbx:provider"}}
	token, key := randomHex(t, 32), "local-"+randomHex(t, 32)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(server, &ledgergrpc.Server{SandboxToken: token, SandboxAccounting: accounting})
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ledger := ledgerv1.NewLedgerServiceClient(conn)
	var mu sync.Mutex
	// This fixture book is updated from explicit external settlement actions,
	// never from Ledger balances, journal queries or snapshot response rows.
	book := sandboxrecon.Snapshot{InstitutionID: institution, TradeID: trade, ClosingMinor: map[string]int64{"NGN": 10000, "USD": 0}}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request sandboxrecon.Request
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 4097))
		if readErr != nil || len(raw) > 4096 {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("X-Osai-Sandbox-Statement-Signature") != sandboxrecon.MAC(raw, key) || json.Unmarshal(raw, &request) != nil || request.InstitutionID != institution || request.TradeID != trade {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		payload, _ := json.Marshal(sandboxrecon.Statement{Snapshot: book, Nonce: request.Nonce, ObservedAt: time.Now().UTC()})
		mu.Unlock()
		json.NewEncoder(w).Encode(sandboxrecon.Envelope{Payload: payload, Signature: sandboxrecon.MAC(payload, key)})
	}))
	defer fixture.Close()
	for k, v := range map[string]string{"OSAI_SANDBOX_ACCOUNTING_POLICY": sandboxaccounting.Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": fixture.URL + "/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": token, "OSAI_TRADE_POSTGRES_DSN": "", "OSAI_SETTLEMENT_POSTGRES_DSN": "", "OSAI_LEDGER_POSTGRES_DSN": "", "OSAI_QUOTE_POSTGRES_DSN": "", "OSAI_RECONCILIATION_POSTGRES_DSN": ""} {
		t.Setenv(k, v)
	}
	client, err := sandboxrecon.New(fixture.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	binding := &sandboxaccounting.PayoutBinding{SettlementID: "sbx:si:" + id, ClientReference: "sbx:payout:" + id, ProviderID: "sbx:provider", BeneficiaryID: "sbx:beneficiary", Currency: "USD", AmountMinor: 95}
	var revision int64
	apply := func(event string) {
		t.Helper()
		command := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, ExpectedRevision: revision, EventID: "sbx:" + id + ":" + event, Event: event, Actor: "sbx:checker", Maker: "sbx:maker", Checker: "sbx:checker", CorrelationID: "sbx:corr", Reason: "explicit statement fixture accounting", Terms: terms, EvidencePayload: `{"fixture":true}`}
		if event == "fx_complete" {
			command.Execution = &fxevidence.FXExecutionEvidence{TradeID: trade, InstitutionID: institution, QuoteID: terms.Plan.QuoteID, Executor: "sbx:executor", SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9500, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: "sbx:fx:" + id, SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
			payload, _ := json.Marshal(command.Execution)
			command.EvidencePayload = string(payload)
		}
		if event == "payout_bind" || event == "payout_submitted" || event == "payout_complete" || event == "return" {
			command.Payout = binding
		}
		hash := sha256.Sum256([]byte(command.EvidencePayload))
		command.EvidenceHash = hex.EncodeToString(hash[:])
		raw, _ := json.Marshal(command)
		if _, err = ledger.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(raw)}); err != nil {
			t.Fatal(event, err)
		}
		revision++
	}
	read := func() sandboxrecon.Snapshot {
		t.Helper()
		response, e := ledger.GetSandboxPosition(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxPositionRequest{InstitutionId: institution, TradeId: trade, Reconciliation: true})
		if e != nil {
			t.Fatal(e)
		}
		var snapshot sandboxrecon.Snapshot
		if json.Unmarshal([]byte(response.ReconciliationJson), &snapshot) != nil {
			t.Fatal("owner snapshot invalid")
		}
		return snapshot
	}
	check := func(wantBreak bool) {
		t.Helper()
		statement, hash, e := client.Read(ctx, institution, trade)
		if e != nil {
			t.Fatal(e)
		}
		breaks, e := service.ReconcileSandboxSnapshot(read(), statement, hash)
		if e != nil || (len(breaks) > 0) != wantBreak {
			t.Fatalf("reconciliation breaks=%d expected_break=%t error=%v", len(breaks), wantBreak, e)
		}
	}
	apply("fund")
	apply("reserve")
	apply("fx_complete")
	mu.Lock()
	book.Movements = []sandboxrecon.Movement{{Reference: "sbx:fx:" + id, Kind: "fx_complete", Currency: "NGN", DeltaMinor: -9500}, {Reference: "sbx:fx:" + id, Kind: "fx_complete", Currency: "USD", DeltaMinor: 95}}
	book.ClosingMinor["NGN"] = 500
	book.ClosingMinor["USD"] = 95
	mu.Unlock()
	check(false)
	apply("payout_bind")
	apply("payout_submitted")
	apply("payout_complete")
	mu.Lock()
	book.Movements = append(book.Movements, sandboxrecon.Movement{Reference: binding.ClientReference, Kind: "payout_complete", Currency: "USD", DeltaMinor: -95})
	book.ClosingMinor["USD"] = 0
	mu.Unlock()
	check(false)
	apply("return")
	mu.Lock()
	book.Movements = append(book.Movements, sandboxrecon.Movement{Reference: binding.ClientReference, Kind: "return", Currency: "USD", DeltaMinor: 95})
	book.ClosingMinor["USD"] = 95
	mu.Unlock()
	check(false)
	apply("release_return")
	apply("refund_fee")
	check(false)
	var journals int
	if err = db.QueryRow(`SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, trade).Scan(&journals); err != nil {
		t.Fatal(err)
	}
	var runCommand func() error
	if os.Getenv("OSAI_SANDBOX_RECONCILIATION_COMMAND_VERIFY") == "true" {
		binary := filepath.Join(t.TempDir(), "reconciliation-fixture.exe")
		build := exec.Command("go", "build", "-p", "1", "-o", binary, "./cmd/reconciliation")
		build.Dir = ".."
		if output, e := build.CombinedOutput(); e != nil {
			t.Fatalf("build reconciliation command: %v %s", e, output)
		}
		runCommand = func() error {
			command := exec.Command(binary)
			command.Env = append(os.Environ(), "OSAI_LEDGER_GRPC_ADDR="+listener.Addr().String(), "OSAI_SANDBOX_RECONCILIATION_URL="+fixture.URL, "OSAI_SANDBOX_RECONCILIATION_KEY="+key, "OSAI_SANDBOX_RECONCILIATION_INSTITUTION="+institution, "OSAI_SANDBOX_RECONCILIATION_TRADE_ID="+trade)
			return command.Run()
		}
		if err := runCommand(); err != nil {
			t.Fatal("actual reconciliation command rejected matching independent statements", err)
		}
	}

	mu.Lock()
	book.Movements[0].DeltaMinor = -9499
	book.Movements = append(book.Movements, book.Movements[1])
	book.ClosingMinor["USD"] = 94
	mu.Unlock()
	check(true)
	service, err = reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	check(true)
	var persisted, after int
	if err = db.QueryRow(`SELECT count(*) FROM reconciliation_breaks WHERE correlation_id=$1 AND state<>'RESOLVED'`, trade).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, trade).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if persisted < 3 || after != journals {
		t.Fatalf("break persistence or unauthorized financial write: breaks=%d journals_before=%d after=%d", persisted, journals, after)
	}
	if service.DirectLedgerWrites() != 0 {
		t.Fatal("reconciliation wrote Ledger")
	}
	t.Log("independent signed fixture statements matched actual Ledger FX source/destination, payout, return and custody closings; mismatch/duplicate/closing breaks survived restart without a balancing journal")
	if runCommand != nil {
		if err := runCommand(); err == nil {
			t.Fatal("actual reconciliation command ignored durable breaks")
		}
		t.Log("actual reconciliation command sandbox mode passed matching statements and refused durable discrepancies")
	}
}
