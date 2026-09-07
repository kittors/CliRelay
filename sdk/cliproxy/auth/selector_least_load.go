package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// LeastLoadSelector sends each request to the account that is currently
// carrying the least work relative to its configured weight.
//
// Weighted round-robin balances the *count* of requests, which is the right
// answer only when every request costs the same. In practice one agent session
// can hold a single account busy for minutes while short requests cycle past
// it, so counting alone still produces a lopsided distribution. This mode
// scores candidates on observed pressure instead:
//
//	score = (recent selection pressure + in-flight requests) / weight
//
// and additionally penalises accounts approaching their quota ceiling, so a
// nearly-exhausted account sheds traffic before it starts returning 429s.
type LeastLoadSelector struct {
	deps *schedulerDeps
}

// quotaLoadPenalty scales a candidate's score as it approaches its quota
// ceiling. At load 0 the score is untouched; at load 1 it is multiplied by
// 1+quotaLoadPenalty, which is enough to move traffic away without hard-banning
// an account that still has headroom.
const quotaLoadPenalty = 4.0

func (s *LeastLoadSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var best *Auth
	bestScore := 0.0
	for _, candidate := range available {
		score := s.score(candidate, now)
		// available is already sorted by ID, so a strict comparison keeps the
		// selection deterministic when several accounts are equally idle.
		if best == nil || score < bestScore {
			best = candidate
			bestScore = score
		}
	}
	if best == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	s.deps.observeSelection(best, now)
	return best, nil
}

func (s *LeastLoadSelector) score(candidate *Auth, now time.Time) float64 {
	weight := authSelectionWeight(candidate)
	if weight <= 0 {
		// getAvailableAuths already filtered these out; guard against a caller
		// handing us a pre-filtered slice that skipped that step.
		weight = 1
	}
	load := s.deps.pressure(candidate, now)
	if s.deps != nil && s.deps.limiter != nil {
		load += float64(s.deps.limiter.GetInFlight(candidate.ID))
	}
	score := load / float64(weight)
	return score * (1 + quotaLoadPenalty*s.deps.loadRatio(candidate))
}
