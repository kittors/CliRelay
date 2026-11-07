package alerting

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Pool capacity answers the question no single account's card can: how much of
// a quota window is left across all the accounts of one provider, when does the
// first of them renew, and at the speed of the last few hours, when would the
// pool run dry. It is computed from the same samples the alert rules judge plus
// the readings the status probes already store, so asking costs no upstream call.

const (
	// PoolLookback is how far back readings are used to estimate how fast a pool
	// is draining. Longer would blend in a quiet night with a busy afternoon;
	// shorter would be a handful of probes.
	PoolLookback = 6 * time.Hour

	// poolMinStretch is the shortest run of readings, counted from the last time
	// the window renewed, that a rate is estimated from. Fewer minutes than this
	// is one probe interval of noise, and "100% in 12 minutes" is not a rate.
	poolMinStretch = 30 * time.Minute

	// poolRenewalRise is how much the share left may go up between two readings
	// before the window is considered to have been refilled. Upstreams report
	// whole percents, so anything above half a point is a real change.
	poolRenewalRise = 0.5

	// poolResetJump is how far the announced reset time may move later before it
	// is called a new cycle. The same upstream announces a reset a second or two
	// apart from probe to probe, while a renewal moves it by a whole window (the
	// shortest of which is hours).
	poolResetJump = 10 * time.Minute

	// poolMinBurn is the slowest drain that is called a drain, in percentage
	// points per hour. Slower than this the pool would outlast any window.
	poolMinBurn = 0.01

	// poolMinCoverage is the share of the accounts with a reading that must also
	// have enough history for a rate before the pool's rate is reported. One
	// account's pace is not the pool's.
	poolMinCoverage = 0.5
)

// PoolReading is one stored reading of one window of one account.
type PoolReading struct {
	At time.Time
	// RemainingPercent is the share left, 0 to 100.
	RemainingPercent float64
	// ResetAt is when that reading said the window would renew; zero when it did not say.
	ResetAt time.Time
}

// PoolSeries names the readings of one window of one account.
type PoolSeries struct {
	Subject string
	Window  string
}

// PoolHistory holds recent readings per window of each account, oldest first.
type PoolHistory map[PoolSeries][]PoolReading

// Pool is what is left of one window across one provider's accounts.
type Pool struct {
	Provider string
	// WindowKey is the upstream's name for the window; WindowLabel its own
	// English name for it (the most common one among the accounts).
	WindowKey   string
	WindowLabel string
	// WindowSeconds is the most common length the accounts report, 0 if none does.
	WindowSeconds int64
	// Accounts is how many accounts of the provider could have this window, and
	// AccountsWithData how many of them have a reading that still describes the
	// current cycle.
	Accounts         int
	AccountsWithData int
	// RemainingPercent is the arithmetic mean of the share left per account with
	// data. Zero when there is none.
	RemainingPercent float64
	// EarliestResetAt is the soonest any account's window renews; zero when
	// unknown.
	EarliestResetAt time.Time
	// BurnPercentPerHour is how many points the mean loses per hour, estimated
	// over the last PoolLookback; nil when there is not enough history.
	BurnPercentPerHour *float64
	// ProjectedExhaustionAt is when the mean would reach zero at that rate. It is
	// set only when that comes before EarliestResetAt: past the first renewal the
	// straight line no longer describes the pool, because that account is refilled.
	ProjectedExhaustionAt time.Time
	// ObservedAt is the oldest observation among the accounts with data: how stale
	// the figure can be.
	ObservedAt time.Time
}

// poolAccumulator collects the accounts of one provider and window.
type poolAccumulator struct {
	members []poolMember
	labels  map[string]int
	spans   map[int64]int
}

type poolMember struct {
	subject string
	window  WindowSample
}

// PoolHistoryScope names the accounts and windows BuildPools will ask the
// history about, so the caller can load exactly those readings.
func PoolHistoryScope(accounts []AccountSample) (subjects, windows []string) {
	seenSubject := map[string]struct{}{}
	seenWindow := map[string]struct{}{}
	for _, account := range accountsBySubject(accounts) {
		if account.StaticKey || account.Subject == "" {
			continue
		}
		for _, w := range account.Windows {
			if !w.AccountWide || strings.TrimSpace(w.Key) == "" {
				continue
			}
			if _, dup := seenSubject[account.Subject]; !dup {
				seenSubject[account.Subject] = struct{}{}
				subjects = append(subjects, account.Subject)
			}
			if _, dup := seenWindow[w.Key]; !dup {
				seenWindow[w.Key] = struct{}{}
				windows = append(windows, w.Key)
			}
		}
	}
	sort.Strings(subjects)
	sort.Strings(windows)
	return subjects, windows
}

// BuildPools adds up the account-wide quota windows of accounts, one pool per
// provider and window. Windows that limit one model or one product are not
// pooled: what is left of them says nothing about whether the provider's
// accounts can serve a request. Static API keys have no window and are left out.
//
// A window that has renewed since it was read is counted as an account without
// data, not as one with 100% left: it is a fresh cycle nobody has measured yet.
func BuildPools(accounts []AccountSample, history PoolHistory, now time.Time) []Pool {
	type key struct{ provider, window string }
	subjectsByProvider := map[string]int{}
	accumulators := map[key]*poolAccumulator{}

	for _, account := range accountsBySubject(accounts) {
		provider := strings.ToLower(strings.TrimSpace(account.Provider))
		if account.StaticKey || provider == "" {
			continue
		}
		subjectsByProvider[provider]++
		seen := map[string]struct{}{}
		for _, w := range account.Windows {
			name := strings.TrimSpace(w.Key)
			if !w.AccountWide || name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			k := key{provider, name}
			acc := accumulators[k]
			if acc == nil {
				acc = &poolAccumulator{labels: map[string]int{}, spans: map[int64]int{}}
				accumulators[k] = acc
			}
			acc.members = append(acc.members, poolMember{subject: account.Subject, window: w})
			if label := strings.TrimSpace(w.Label); label != "" {
				acc.labels[label]++
			}
			acc.spans[int64(w.Span/time.Second)]++
		}
	}

	pools := make([]Pool, 0, len(accumulators))
	for k, acc := range accumulators {
		pool := Pool{
			Provider: k.provider, WindowKey: k.window,
			WindowLabel: commonLabel(acc.labels), WindowSeconds: commonSpan(acc.spans),
			Accounts: subjectsByProvider[k.provider],
		}
		var remaining, rates []float64
		for _, m := range acc.members {
			// A reading whose window has renewed describes a cycle that is over.
			if !m.window.ResetAt.IsZero() && !m.window.ResetAt.After(now) {
				continue
			}
			remaining = append(remaining, clampPercent(100-m.window.UsedPercent))
			if !m.window.ResetAt.IsZero() && (pool.EarliestResetAt.IsZero() || m.window.ResetAt.Before(pool.EarliestResetAt)) {
				pool.EarliestResetAt = m.window.ResetAt.UTC()
			}
			if !m.window.ObservedAt.IsZero() && (pool.ObservedAt.IsZero() || m.window.ObservedAt.Before(pool.ObservedAt)) {
				pool.ObservedAt = m.window.ObservedAt.UTC()
			}
			if rate, ok := drainRate(history[PoolSeries{Subject: m.subject, Window: k.window}], now); ok {
				rates = append(rates, rate)
			}
		}
		pool.AccountsWithData = len(remaining)
		if pool.AccountsWithData > 0 {
			pool.RemainingPercent = roundTo(mean(remaining), 1)
			if float64(len(rates)) >= poolMinCoverage*float64(pool.AccountsWithData) {
				burn := roundTo(mean(rates), 2)
				pool.BurnPercentPerHour = &burn
				pool.ProjectedExhaustionAt = projectExhaustion(pool.RemainingPercent, burn, pool.EarliestResetAt, now)
			}
		}
		pools = append(pools, pool)
	}
	sort.Slice(pools, func(i, j int) bool {
		if pools[i].Provider != pools[j].Provider {
			return pools[i].Provider < pools[j].Provider
		}
		return pools[i].WindowKey < pools[j].WindowKey
	})
	return pools
}

// projectExhaustion extends the straight line of the last hours to zero. It
// answers only when that comes before the first renewal in the pool, and only
// for a pool that is not already empty or still: a slower drain than poolMinBurn
// would outlast every window.
func projectExhaustion(remaining, burn float64, earliestReset, now time.Time) time.Time {
	if remaining <= 0 || burn < poolMinBurn || earliestReset.IsZero() {
		return time.Time{}
	}
	at := now.Add(time.Duration(remaining / burn * float64(time.Hour)))
	if !at.Before(earliestReset) {
		return time.Time{}
	}
	return at.UTC()
}

// drainRate estimates how many points per hour one window lost, from the
// readings since it last renewed. ok is false when there are too few of them to
// say.
//
// The renewal is found in the readings themselves: the share left going up, or
// the announced reset moving a whole window later. Counting across it would turn
// a refill into a negative drain and hide a busy cycle behind it.
func drainRate(readings []PoolReading, now time.Time) (rate float64, ok bool) {
	recent := make([]PoolReading, 0, len(readings))
	for _, r := range readings {
		if r.At.After(now) || now.Sub(r.At) > PoolLookback {
			continue
		}
		recent = append(recent, r)
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].At.Before(recent[j].At) })
	start := 0
	for i := 1; i < len(recent); i++ {
		rose := recent[i].RemainingPercent > recent[i-1].RemainingPercent+poolRenewalRise
		moved := !recent[i].ResetAt.IsZero() && !recent[i-1].ResetAt.IsZero() &&
			recent[i].ResetAt.Sub(recent[i-1].ResetAt) > poolResetJump
		if rose || moved {
			start = i
		}
	}
	cycle := recent[start:]
	if len(cycle) < 2 {
		return 0, false
	}
	first, last := cycle[0], cycle[len(cycle)-1]
	stretch := last.At.Sub(first.At)
	if stretch < poolMinStretch {
		return 0, false
	}
	return math.Max(0, first.RemainingPercent-last.RemainingPercent) / stretch.Hours(), true
}

func clampPercent(v float64) float64 { return math.Min(100, math.Max(0, v)) }

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// commonLabel is the label most accounts use; ties go to the alphabetically
// first so the answer does not depend on map order.
func commonLabel(votes map[string]int) string {
	best, bestVotes := "", 0
	for label, n := range votes {
		if n > bestVotes || (n == bestVotes && label < best) {
			best, bestVotes = label, n
		}
	}
	return best
}

// commonSpan is the window length most accounts report, the longer on a tie. A
// length of 0 means "not said" and only wins when nobody says anything else.
func commonSpan(votes map[int64]int) int64 {
	var best int64
	bestVotes := 0
	for span, n := range votes {
		if span == 0 {
			continue
		}
		if n > bestVotes || (n == bestVotes && span > best) {
			best, bestVotes = span, n
		}
	}
	return best
}
