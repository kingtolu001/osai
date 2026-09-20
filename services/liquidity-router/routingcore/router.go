package routingcore

import (
	"fmt"
	"sort"

	quotecore "github.com/osai/osai/services/quote/quotecore"
)

// ProviderProfile is the normalized capability metadata used for deterministic routing.
type ProviderProfile struct {
	ID                         string
	Healthy                    bool
	Reliability                int
	DepthMinor                 int64
	SettlementCompatible       bool
	HealthScore                int
	ExposureMinor              int64
	PolicyWeight               int
	AllInCostMinor             int64
	CorridorOverrideMultiplier int
}

// RouteCandidate is a deterministic route recommendation.
type RouteCandidate struct {
	ProviderID string
	Score      int
	AllInCost  int64
	Depth      int64
	Reasons    []string
}

// RankProviders filters ineligible providers and ranks by deterministic weighted score.
func RankProviders(req quotecore.QuoteRequest, providers []ProviderProfile) ([]RouteCandidate, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers available for %s->%s", req.BaseCurrency, req.QuoteCurrency)
	}

	candidates := make([]RouteCandidate, 0, len(providers))
	for _, provider := range providers {
		if !provider.Healthy {
			candidates = append(candidates, RouteCandidate{ProviderID: provider.ID, Score: -1_000_000, AllInCost: provider.AllInCostMinor, Depth: provider.DepthMinor, Reasons: []string{"provider health exclusion"}})
			continue
		}
		if !provider.SettlementCompatible {
			candidates = append(candidates, RouteCandidate{ProviderID: provider.ID, Score: -1_000_000, AllInCost: provider.AllInCostMinor, Depth: provider.DepthMinor, Reasons: []string{"unsupported settlement corridor"}})
			continue
		}
		if provider.ExposureMinor > 0 && provider.PolicyWeight > 0 {
			// policy weight can be used to penalize concentration; deterministic but auditable.
		}
		score := provider.BaseScore() - int(provider.AllInCostMinor/10) + provider.Reliability*20 + int(provider.DepthMinor/1000)
		score += provider.CorridorOverrideMultiplier
		score -= int(provider.ExposureMinor / 1000)
		score -= provider.PolicyWeight * 25
		if req.BaseCurrency == req.QuoteCurrency {
			score += 50
		}
		candidates = append(candidates, RouteCandidate{
			ProviderID: provider.ID,
			Score:      score,
			AllInCost:  provider.AllInCostMinor,
			Depth:      provider.DepthMinor,
			Reasons: []string{
				fmt.Sprintf("all_in_cost=%d", provider.AllInCostMinor),
				fmt.Sprintf("reliability=%d", provider.Reliability),
				fmt.Sprintf("depth=%d", provider.DepthMinor),
				"policy-weighted-score",
			},
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			if candidates[i].AllInCost == candidates[j].AllInCost {
				return candidates[i].ProviderID < candidates[j].ProviderID
			}
			return candidates[i].AllInCost < candidates[j].AllInCost
		}
		return candidates[i].Score > candidates[j].Score
	})

	return candidates, nil
}

func (p ProviderProfile) BaseScore() int {
	base := 100
	base += p.HealthScore * 2
	base += p.Reliability * 3
	if p.SettlementCompatible {
		base += 35
	}
	return base
}
