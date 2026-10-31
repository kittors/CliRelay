// Package inflight keeps what admitted requests are expected to spend until
// their usage is in the usage database.
//
// A budget check compares the usage already recorded with the limit. Requests
// that have been admitted but not billed yet are invisible to it, so N
// concurrent requests of a key one dollar short of its limit are all admitted
// and the key ends N requests over. Each admitted request therefore holds a
// reservation, and a check counts the reservations admitted before it: a
// request still sees exactly what it saw before when it runs alone, and a
// burst near the limit overshoots by at most about one request.
//
// A reservation is an estimate: one request, and the tokens and cost an
// average request of the same key has recently billed. It is estimated from
// the key's billed history rather than from the request body on purpose: most
// coding-agent traffic is billed at cache-read prices, so a byte count priced
// at the input rate would reserve several times what a request costs and
// refuse keys long before they reach their limit.
//
// A reservation lasts while its request runs and until every usage record the
// request published has been committed, which is the moment the checks start
// counting the real amounts. A request that published nothing (refused before
// it reached an upstream) is released when it ends. A record that is never
// committed (the database is down) holds its request for SettleAfter at most,
// so an outage cannot hold a key's budget.
//
// The ledger is per process. In a cluster each node sees only its own
// requests in flight; the recorded usage the checks read is shared.
package inflight

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// DefaultSettleAfter bounds how long a finished request waits for its usage
// records to be committed. The usage worker commits a record within
// milliseconds of the request ending; the bound only matters when the
// database is slow or down.
const DefaultSettleAfter = 10 * time.Second

// averageWeight is the weight of the newest billed request in a key's running
// average. 0.2 follows a change of workload within about ten requests without
// letting one unusually large request dominate.
const averageWeight = 0.2

// Amounts is what a reservation, or a sum of them, stands for.
type Amounts struct {
	Requests float64
	Tokens   float64
	Cost     float64
}

// IsZero reports whether nothing is reserved.
func (a Amounts) IsZero() bool { return a.Requests == 0 && a.Tokens == 0 && a.Cost == 0 }

func (a Amounts) add(b Amounts) Amounts {
	return Amounts{Requests: a.Requests + b.Requests, Tokens: a.Tokens + b.Tokens, Cost: a.Cost + b.Cost}
}

// Matrix spreads the amounts over every window of a usage matrix: a request in
// flight counts toward every window, since each of them contains the present.
func (a Amounts) Matrix() quota.Matrix {
	var m quota.Matrix
	for _, window := range quota.OrderedWindows {
		m.Set(quota.MetricRequests, window, a.Requests)
		m.Set(quota.MetricTokens, window, a.Tokens)
		m.Set(quota.MetricCost, window, a.Cost)
	}
	return m
}

// Usage is the amounts as the usage views show them; nil when nothing is held.
func (a Amounts) Usage() *quota.InFlightUsage {
	if a.Requests <= 0 {
		return nil
	}
	return &quota.InFlightUsage{Requests: int64(a.Requests + 0.5), Tokens: int64(a.Tokens + 0.5), Cost: a.Cost}
}

// KeyName names the budgets of one API key. Key ids are unique across tenants.
func KeyName(apiKeyID string) string { return "key:" + strings.TrimSpace(apiKeyID) }

// AccountName names the budgets of one end-user account, which all its keys
// share. Account ids are unique across tenants.
func AccountName(endUserID string) string { return "account:" + strings.TrimSpace(endUserID) }

// Owner names the budgets a request draws on.
type Owner struct {
	// Key names the key's own budgets (see KeyName).
	Key string
	// Account names the budgets of the account the key belongs to (see
	// AccountName); empty for a key without an account.
	Account string
}

func (o Owner) name(scope Scope) string {
	if scope == ScopeAccount {
		return o.Account
	}
	return o.Key
}

// Scope selects which of its owner's budgets a sum is taken over.
type Scope uint8

const (
	// ScopeKey sums the requests of the same key.
	ScopeKey Scope = iota
	// ScopeAccount sums the requests of every key of the same account.
	ScopeAccount
)

type reservationState uint8

const (
	stateRunning reservationState = iota
	// stateFinished: the request ended and some of its usage records have not
	// been committed yet.
	stateFinished
	stateReleased
)

// Reservation is one admitted request's hold on its budgets.
type Reservation struct {
	ledger    *Ledger
	seq       uint64
	requestID string
	owner     Owner
	amounts   Amounts

	// The fields below are guarded by the ledger's mutex.
	model string
	state reservationState
	// pending counts the request's usage records that were published and not
	// committed yet.
	pending int
	timer   *time.Timer
}

// Amounts returns what the reservation holds.
func (r *Reservation) Amounts() Amounts {
	if r == nil {
		return Amounts{}
	}
	return r.amounts
}

type average struct {
	cost, tokens float64
	seen         bool
}

func (a *average) observe(cost, tokens float64) {
	if !a.seen {
		a.cost, a.tokens, a.seen = cost, tokens, true
		return
	}
	a.cost += averageWeight * (cost - a.cost)
	a.tokens += averageWeight * (tokens - a.tokens)
}

// Ledger holds the reservations of one process.
type Ledger struct {
	mu     sync.Mutex
	seq    uint64
	byName map[string]map[uint64]*Reservation
	// byRequest lists the reservations of each request id in admission order.
	// Turns of one WebSocket connection share the connection's request id.
	byRequest map[string][]*Reservation
	averages  map[string]*average
	overall   average

	settleAfter time.Duration
}

// NewLedger returns an empty ledger that waits settleAfter at most for a
// finished request's usage records.
func NewLedger(settleAfter time.Duration) *Ledger {
	if settleAfter <= 0 {
		settleAfter = DefaultSettleAfter
	}
	return &Ledger{
		byName:      map[string]map[uint64]*Reservation{},
		byRequest:   map[string][]*Reservation{},
		averages:    map[string]*average{},
		settleAfter: settleAfter,
	}
}

var defaultLedger = NewLedger(DefaultSettleAfter)

// Default returns the process-wide ledger the gateway admits requests against.
func Default() *Ledger { return defaultLedger }

// Reserve registers an admitted request. The estimate comes from the key's
// billed history, else the account's, else the whole process's. requestID ties
// the reservation to the request's usage records; without one, the request is
// released when it ends.
func (l *Ledger) Reserve(requestID string, owner Owner) *Reservation {
	if l == nil {
		return nil
	}
	owner = Owner{Key: strings.TrimSpace(owner.Key), Account: strings.TrimSpace(owner.Account)}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	r := &Reservation{
		ledger:    l,
		seq:       l.seq,
		requestID: strings.TrimSpace(requestID),
		owner:     owner,
		amounts:   l.estimateLocked(owner),
	}
	for _, name := range []string{owner.Key, owner.Account} {
		if name == "" {
			continue
		}
		held := l.byName[name]
		if held == nil {
			held = map[uint64]*Reservation{}
			l.byName[name] = held
		}
		held[r.seq] = r
	}
	if r.requestID != "" {
		l.byRequest[r.requestID] = append(l.byRequest[r.requestID], r)
	}
	return r
}

func (l *Ledger) estimateLocked(owner Owner) Amounts {
	estimate := Amounts{Requests: 1}
	for _, name := range []string{owner.Key, owner.Account} {
		if avg := l.averages[name]; name != "" && avg != nil {
			estimate.Cost, estimate.Tokens = avg.cost, avg.tokens
			return estimate
		}
	}
	if l.overall.seen {
		estimate.Cost, estimate.Tokens = l.overall.cost, l.overall.tokens
	}
	return estimate
}

// Before sums what the requests admitted before r still hold in scope. A nil
// reservation sees nothing, which is what a check outside an admission (a
// WebSocket handshake) should see.
func (r *Reservation) Before(scope Scope) Amounts {
	return r.before(scope, nil)
}

// BeforeMatching is Before restricted to requests routed to a model matching
// one of patterns, for rules that cap particular models. A request whose model
// is not known yet is left out.
func (r *Reservation) BeforeMatching(scope Scope, patterns []string) Amounts {
	return r.before(scope, func(model string) bool {
		return model != "" && quota.MatchesAnyModelPattern(patterns, model)
	})
}

func (r *Reservation) before(scope Scope, keep func(model string) bool) Amounts {
	if r == nil || r.ledger == nil {
		return Amounts{}
	}
	name := r.owner.name(scope)
	if name == "" {
		return Amounts{}
	}
	l := r.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum Amounts
	for seq, other := range l.byName[name] {
		if seq >= r.seq || (keep != nil && !keep(other.model)) {
			continue
		}
		sum = sum.add(other.amounts)
	}
	return sum
}

// SetModel records the model the request was routed to, once it is known.
func (r *Reservation) SetModel(model string) {
	if r == nil || r.ledger == nil {
		return
	}
	r.ledger.mu.Lock()
	r.model = quota.NormalizeModelName(model)
	r.ledger.mu.Unlock()
}

// Cancel drops the reservation of a request that was not admitted after all.
func (r *Reservation) Cancel() {
	if r == nil || r.ledger == nil {
		return
	}
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.releaseLocked(r)
}

// Finish marks the request as ended. Its reservation is released at once when
// none of its usage records is still waiting to be committed, else when the
// last of them is or after the ledger's settle bound, whichever comes first.
func (r *Reservation) Finish() {
	if r == nil || r.ledger == nil {
		return
	}
	l := r.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.state != stateRunning {
		return
	}
	if r.pending == 0 {
		l.releaseLocked(r)
		return
	}
	r.state = stateFinished
	r.timer = time.AfterFunc(l.settleAfter, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releaseLocked(r)
	})
}

// Published notes that a usage record of requestID was queued for the
// database; the request keeps its reservation until the record is committed.
func (l *Ledger) Published(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if l == nil || requestID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reservations := l.byRequest[requestID]
	if len(reservations) == 0 {
		return
	}
	// The record comes from the turn that is running. One published after its
	// request ended (a stream winding down after the client left) goes to the
	// latest request that is still waiting.
	target := reservations[len(reservations)-1]
	for _, r := range reservations {
		if r.state == stateRunning {
			target = r
			break
		}
	}
	target.pending++
}

// Settle takes a usage record of requestID that was committed, or dropped for
// good. billed marks a committed record of a successful request: its cost and
// tokens feed the running averages. A failed attempt (answered with an error,
// usually costing nothing, and possibly retried) or a dropped record does not.
func (l *Ledger) Settle(requestID string, billed bool, cost float64, tokens int64) {
	requestID = strings.TrimSpace(requestID)
	if l == nil || requestID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reservations := l.byRequest[requestID]
	if len(reservations) == 0 {
		return
	}
	// The usage worker commits records in the order they were published, so
	// this one belongs to the earliest request still waiting for a record.
	target := reservations[0]
	for _, r := range reservations {
		if r.pending > 0 {
			target = r
			break
		}
	}
	if billed {
		l.observeLocked(target.owner, max(cost, 0), float64(max(tokens, 0)))
	}
	if target.pending == 0 {
		return
	}
	target.pending--
	if target.pending == 0 && target.state == stateFinished {
		l.releaseLocked(target)
	}
}

func (l *Ledger) observeLocked(owner Owner, cost, tokens float64) {
	for _, name := range []string{owner.Key, owner.Account} {
		if name == "" {
			continue
		}
		avg := l.averages[name]
		if avg == nil {
			avg = &average{}
			l.averages[name] = avg
		}
		avg.observe(cost, tokens)
	}
	l.overall.observe(cost, tokens)
}

func (l *Ledger) releaseLocked(r *Reservation) {
	if r.state == stateReleased {
		return
	}
	r.state = stateReleased
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	for _, name := range []string{r.owner.Key, r.owner.Account} {
		if held := l.byName[name]; held != nil {
			delete(held, r.seq)
			if len(held) == 0 {
				delete(l.byName, name)
			}
		}
	}
	if r.requestID == "" {
		return
	}
	reservations := l.byRequest[r.requestID]
	for i, other := range reservations {
		if other == r {
			reservations = append(reservations[:i:i], reservations[i+1:]...)
			break
		}
	}
	if len(reservations) == 0 {
		delete(l.byRequest, r.requestID)
	} else {
		l.byRequest[r.requestID] = reservations
	}
}

// Held sums every reservation under name (see KeyName and AccountName), for
// showing what a key or account has in flight.
func (l *Ledger) Held(name string) Amounts {
	if l == nil {
		return Amounts{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum Amounts
	for _, r := range l.byName[strings.TrimSpace(name)] {
		sum = sum.add(r.amounts)
	}
	return sum
}

// names lists the names holding reservations, sorted; for tests.
func (l *Ledger) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.byName))
	for name := range l.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
