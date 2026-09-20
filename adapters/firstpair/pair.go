package firstpair

import "github.com/osai/osai/adapters/simulator/simcore"

// Pair is the controlled Phase 3 sandbox pair: the authorised simulator LP and rail.
// A production credentialed adapter must replace this pair without changing core contracts.
type Pair struct {
	Liquidity  *simcore.NormalizedAdapter
	Settlement *simcore.NormalizedAdapter
}

func New() Pair {
	adapter := simcore.NewNormalizedAdapter(simcore.FailureProfile{})
	return Pair{Liquidity: adapter, Settlement: adapter}
}
