package tradecore

import (
	"fmt"
	"time"

	"github.com/osai/osai/services/liquidity-router/routingcore"
	quotecore "github.com/osai/osai/services/quote/quotecore"
)

// SimulatedTradeExecution represents the end-to-end simulated flow used in Phase 2.
type SimulatedTradeExecution struct {
	TradeID        string
	QuoteID        string
	ProviderID     string
	RouteScore     int
	FundingMinor   int64
	ExecutionMinor int64
	State          TradeState
	StartedAt      time.Time
	CompletedAt    *time.Time
}

func ExecuteSimulatedTrade(quote *quotecore.ExecutableQuote, route routingcore.RouteCandidate, fundingMinor int64) (*SimulatedTradeExecution, error) {
	if quote == nil {
		return nil, fmt.Errorf("quote is required")
	}
	if quote.Status != quotecore.QuoteStatusAccepted {
		return nil, fmt.Errorf("quote must be accepted before execution")
	}
	if fundingMinor < 0 {
		return nil, fmt.Errorf("funding minor cannot be negative")
	}

	execution := &SimulatedTradeExecution{
		TradeID:        quote.AcceptedTradeID,
		QuoteID:        quote.ID,
		ProviderID:     route.ProviderID,
		RouteScore:     route.Score,
		FundingMinor:   fundingMinor,
		ExecutionMinor: quote.AmountOutMinor,
		State:          StateAccepted,
		StartedAt:      time.Now().UTC(),
	}
	if execution.ExecutionMinor <= 0 {
		execution.ExecutionMinor = 0
	}
	completedAt := time.Now().UTC()
	execution.CompletedAt = &completedAt
	execution.State = StateFunded
	return execution, nil
}
