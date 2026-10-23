package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/correlation"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type storageFaultTrade struct {
	tradev1.TradeServiceClient
	calls               int
	outgoingCorrelation string
	beforeReply         func() error
}

func (c *storageFaultTrade) CreateTradeFromAcceptedQuote(ctx context.Context, _ *tradev1.CreateTradeFromAcceptedQuoteRequest, _ ...grpc.CallOption) (*tradev1.TradeResponse, error) {
	c.calls++
	md, _ := metadata.FromOutgoingContext(ctx)
	if values := md.Get(correlation.HeaderName); len(values) == 1 {
		c.outgoingCorrelation = values[0]
	}
	if c.beforeReply != nil {
		if err := c.beforeReply(); err != nil {
			return nil, err
		}
	}
	return &tradev1.TradeResponse{TradeId: "storage-fault-trade"}, nil
}

func TestPostgresAcceptanceStorageFailuresRemainRetryable(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL not configured")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "quote_fault_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			db.Close()
		}
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err = sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	store, err := quotecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	policy := quoteTestPolicy("inst-storage-fault")
	policy.SourceCurrency, policy.DestinationCurrency = "NGN", "NGN"
	request := quotecore.QuoteRequest{CustomerID: policy.InstitutionID, QuoteMode: quotecore.QuoteModeFixedSource, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "storage-fault"}
	priced, err := quotecore.PriceQuote(policy, quotecore.QuotePricingInput{Mode: request.QuoteMode, SourceAmountMinor: 10000, RateScaled: quotecore.RateScale})
	if err != nil {
		t.Fatal(err)
	}
	q, err := store.CreatePricedFromProvider(request, quotecore.ProviderQuote{ProviderID: "sim_lp_1", BaseCurrency: "NGN", QuoteCurrency: "NGN", BaseAmountMinor: priced.SourcePrincipalMinor, RateMinor: quotecore.RateScale, AmountOutMinor: priced.DestinationMinor, ExpiresAt: time.Now().Add(time.Hour)}, policy, priced)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE quote_write_fault(enabled boolean NOT NULL);
 INSERT INTO quote_write_fault VALUES(true);
 CREATE FUNCTION reject_quote_fixture_write() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF (SELECT enabled FROM quote_write_fault LIMIT 1) THEN
 RAISE EXCEPTION 'fixture-diagnostic-do-not-expose'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER quote_fixture_write_fault BEFORE UPDATE ON quotes FOR EACH ROW EXECUTE FUNCTION reject_quote_fixture_write();`)
	if err != nil {
		t.Fatal(err)
	}
	client := &storageFaultTrade{}
	server := &grpcServer{store: store, tradeClient: client}
	accept := &quotev1.AcceptQuoteRequest{InstitutionId: policy.InstitutionID, QuoteId: q.ID, IdempotencyKey: "accept-storage", BeneficiaryId: "beneficiary", CorrelationId: "original-correlation"}
	assertRetryable := func(err error) {
		t.Helper()
		if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "fixture-diagnostic-do-not-expose") {
			t.Fatal("storage failure was not safely classified as retryable")
		}
	}
	_, err = server.AcceptQuote(context.Background(), accept)
	assertRetryable(err)
	if client.calls != 0 {
		t.Fatal("Trade called before selection was durable")
	}
	if _, err = db.Exec(`UPDATE quote_write_fault SET enabled=false`); err != nil {
		t.Fatal(err)
	}
	client.beforeReply = func() error { _, err := db.Exec(`UPDATE quote_write_fault SET enabled=true`); return err }
	_, err = server.AcceptQuote(context.Background(), accept)
	assertRetryable(err)
	if client.calls != 1 {
		t.Fatal("expected one acknowledged Trade call")
	}
	claimed, ok := store.Get(q.ID)
	if !ok || claimed.Status != quotecore.QuoteStatusQuoted || claimed.AcceptanceStartedAt == nil || claimed.AcceptanceKey != accept.IdempotencyKey {
		t.Fatal("acknowledged Trade outcome lost its durable selection")
	}
	if _, err = db.Exec(`UPDATE quote_write_fault SET enabled=false`); err != nil {
		t.Fatal(err)
	}
	client.beforeReply = nil
	server.store, err = quotecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(claimed.AcceptanceStartedAt.Add(5*time.Second + 50*time.Millisecond)); delay > 0 {
		<-time.After(delay)
	}
	processed, err := server.recoverPendingAcceptances(context.Background(), 1)
	recovered, recoveredOK := server.store.Get(q.ID)
	if err != nil || processed != 1 || !recoveredOK || recovered.AcceptedTradeID != "storage-fault-trade" || client.calls != 2 {
		t.Fatal("background storage recovery did not acknowledge the original trade", err)
	}
	if client.outgoingCorrelation != "original-correlation" {
		t.Fatal("background recovery lost original trace correlation")
	}
	var events int
	if err = db.QueryRow(`SELECT count(*) FROM quote_outbox WHERE event_id=$1`, "evt_quote_accepted_"+q.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("acceptance event missing or duplicated", err)
	}
	lock, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM quotes WHERE id=$1 FOR UPDATE`, q.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = store.BeginAcceptanceContext(ctx, q.ID, accept.IdempotencyKey, accept.BeneficiaryId, accept.CorrelationId, time.Now())
	if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() != context.DeadlineExceeded {
		t.Fatal("blocked acceptance ignored request cancellation", err)
	}
}
