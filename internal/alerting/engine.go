package alerting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Sources provides what the rules judge. The management layer implements it on
// top of stored data (the account status, the usage tables); the engine and the
// rules know nothing about storage, which keeps them testable and keeps
// anything on the request path out of this package.
type Sources interface {
	// Accounts returns the tenant's upstream accounts that are switched on.
	Accounts(ctx context.Context, tenantID string, now time.Time) ([]AccountSample, error)
	// Spenders returns the tenant's keys and end users that have a limit, with
	// how much of it they have used.
	Spenders(ctx context.Context, tenantID string, now time.Time) ([]SpendSample, error)
}

// Sender delivers a message to channels. *Dispatcher is the production one.
type Sender interface {
	Send(ctx context.Context, channels []Channel, msg Message, trusted bool) []Delivery
}

const (
	// DefaultInterval is how often the loop looks. Everything it reads is already
	// stored (the status probes run every 15 minutes), so looking more often
	// would only find the same things sooner by seconds.
	DefaultInterval = time.Minute
	// DefaultStartDelay lets a starting process settle before the first round: the
	// auth manager is still loading credentials, and the previous backend slot
	// (which is still evaluating during a deploy) has just handed over.
	DefaultStartDelay = 90 * time.Second

	// spendingEvery is how often a tenant's key and end-user spending is read. It
	// is the one rule that queries usage tables rather than the small status row.
	spendingEvery = 5 * time.Minute

	// tenantBudget bounds the work for one tenant in one round, delivery
	// included. What does not fit is simply not claimed and is found again next
	// round.
	tenantBudget = 3 * time.Minute

	// resetGrace is added to a window's reset time when deciding how long its
	// alert stays quiet: reset times drift by a second or two between probes, and
	// for a few minutes after a reset the stored reading is still the old one.
	resetGrace = 15 * time.Minute

	// groupAbove is how many problems of one rule found in one round are still
	// sent as separate messages. More than that become one summary.
	groupAbove = 3
	// summaryLines is how many items a summary lists in its text. The payload
	// carries them all, up to maxItems.
	summaryLines = 10
	maxItems     = 50
)

// ruleOrder is the order the rules of a round are sent in: what needs a person
// first.
var ruleOrder = []string{RuleAccountFailure, RuleQuotaWindow, RuleSpending, RuleUseItOrLoseIt}

// Engine evaluates the rules of every tenant that has alerting on and delivers
// what they find. It runs on one node of a cluster (the leader), on its own
// goroutine, from stored data: nothing it does is on the request path, and a
// round that finds nothing costs one settings query.
type Engine struct {
	store      Store
	sources    Sources
	sender     Sender
	isLeader   func() bool
	now        func() time.Time
	interval   time.Duration
	startDelay time.Duration

	mu sync.Mutex
	// failing is, per tenant, when each account was first seen failing (see
	// failureCandidates). It lives in memory on purpose.
	failing map[string]map[string]time.Time
	// spendRead is when each tenant's spending was last read.
	spendRead map[string]time.Time
	cancel    context.CancelFunc
}

// EngineOptions configures NewEngine.
type EngineOptions struct {
	Store   Store
	Sources Sources
	Sender  Sender
	// IsLeader says whether this node runs the loop. Nil means always.
	IsLeader   func() bool
	Interval   time.Duration
	StartDelay time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NewEngine builds an engine. It does nothing until Start (or RunOnce).
func NewEngine(opts EngineOptions) *Engine {
	e := &Engine{
		store: opts.Store, sources: opts.Sources, sender: opts.Sender,
		isLeader: opts.IsLeader, now: opts.Now,
		interval: opts.Interval, startDelay: opts.StartDelay,
		failing: map[string]map[string]time.Time{}, spendRead: map[string]time.Time{},
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e
}

// Start runs rounds until Stop. Starting a running engine restarts it.
func (e *Engine) Start() {
	if e == nil || e.store == nil || e.sources == nil || e.sender == nil || e.interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	previous := e.cancel
	e.cancel = cancel
	e.mu.Unlock()
	if previous != nil {
		previous()
	}
	go e.loop(ctx)
}

// Stop halts the loop. A round in progress finishes the delivery it is making.
func (e *Engine) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *Engine) loop(ctx context.Context) {
	if e.startDelay > 0 {
		timer := time.NewTimer(e.startDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		// Only the leader evaluates: two nodes would send every alert twice. A
		// single node is always the leader.
		if e.isLeader == nil || e.isLeader() {
			e.RunOnce(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce evaluates every tenant that has alerting on, once.
func (e *Engine) RunOnce(ctx context.Context) {
	if e == nil || e.store == nil || e.sources == nil || e.sender == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("alerting: evaluation round failed: %v", recovered)
		}
	}()
	all, err := e.store.SettingsOfAll()
	if err != nil {
		log.Debugf("alerting: could not read the settings of the tenants: %v", err)
		return
	}
	active := make(map[string]struct{}, len(all))
	for _, entry := range all {
		settings := entry.Settings.Normalized()
		if !settings.Active() || !settings.RulesEnabled() {
			continue
		}
		active[entry.TenantID] = struct{}{}
		if ctx.Err() != nil {
			return
		}
		e.runTenant(ctx, entry.TenantID, settings)
	}
	e.forgetTenants(active)
}

// forgetTenants drops the in-memory clocks of tenants that no longer have
// alerting on, so switching it off and on again starts from nothing.
func (e *Engine) forgetTenants(active map[string]struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for tenantID := range e.failing {
		if _, ok := active[tenantID]; !ok {
			delete(e.failing, tenantID)
		}
	}
	for tenantID := range e.spendRead {
		if _, ok := active[tenantID]; !ok {
			delete(e.spendRead, tenantID)
		}
	}
}

func (e *Engine) runTenant(ctx context.Context, tenantID string, settings Settings) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// One tenant's data must not stop the others from being evaluated.
			log.Errorf("alerting: evaluating tenant %s failed: %v", tenantID, recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, tenantBudget)
	defer cancel()
	now := e.now()
	rules := settings.Rules

	var candidates []Candidate
	// failing stays nil unless failures were actually evaluated this round: a
	// round that could not read the accounts must not conclude that they all
	// recovered.
	var failing map[string]struct{}
	if rules.QuotaWindow.Enabled || rules.AccountFailure.Enabled || rules.UseItOrLoseIt.Enabled {
		accounts, err := e.sources.Accounts(ctx, tenantID, now)
		if err != nil {
			log.Debugf("alerting: could not read the accounts of tenant %s: %v", tenantID, err)
		} else {
			if rules.QuotaWindow.Enabled {
				candidates = append(candidates, quotaCandidates(rules.QuotaWindow, accounts, now)...)
			}
			if rules.UseItOrLoseIt.Enabled {
				candidates = append(candidates, useItOrLoseItCandidates(rules.UseItOrLoseIt, accounts, now)...)
			}
			if rules.AccountFailure.Enabled {
				found, still := e.failures(tenantID, rules.AccountFailure, accounts, now)
				candidates = append(candidates, found...)
				failing = still
			}
		}
	}
	if !rules.AccountFailure.Enabled {
		e.mu.Lock()
		delete(e.failing, tenantID)
		e.mu.Unlock()
	}
	if rules.Spending.Enabled && e.spendingDue(tenantID, now) {
		spenders, err := e.sources.Spenders(ctx, tenantID, now)
		if err != nil {
			log.Debugf("alerting: could not read the spending of tenant %s: %v", tenantID, err)
		} else {
			candidates = append(candidates, spendingCandidates(rules.Spending, spenders, now)...)
			e.markSpendRead(tenantID, now)
		}
	}
	e.deliver(ctx, tenantID, settings, candidates, failing, now)
}

func (e *Engine) failures(tenantID string, rule AccountFailureRule, accounts []AccountSample, now time.Time) ([]Candidate, map[string]struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	since := e.failing[tenantID]
	if since == nil {
		since = make(map[string]time.Time)
		e.failing[tenantID] = since
	}
	return failureCandidates(rule, accounts, since, now)
}

func (e *Engine) spendingDue(tenantID string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	last, ok := e.spendRead[tenantID]
	return !ok || now.Sub(last) >= spendingEvery
}

func (e *Engine) markSpendRead(tenantID string, now time.Time) {
	e.mu.Lock()
	e.spendRead[tenantID] = now
	e.mu.Unlock()
}

// plannedMessage is a message about to be sent and the problems it covers.
type plannedMessage struct {
	rule  string
	msg   Message
	cands []Candidate
	// skipped is set when the tenant's time ran out before this message was sent.
	skipped bool
	ok      bool
}

// deliver turns the round's candidates into messages and sends them.
//
// The order matters for the two backend slots that overlap during a deploy, and
// for a leader that changes mid-round. The problems about to be reported are
// first reserved in the sent table with a version-checked write; only the
// process whose write succeeds sends. After delivery the reservation is replaced
// by the real quiet period, or by a retry time if no channel accepted the
// message.
func (e *Engine) deliver(ctx context.Context, tenantID string, settings Settings, candidates []Candidate, failing map[string]struct{}, now time.Time) {
	sent, version, err := e.store.LoadSent(tenantID)
	if err != nil {
		log.Debugf("alerting: could not read what was already sent to tenant %s: %v", tenantID, err)
		return
	}
	before := len(sent.Records)
	sent.prune(now)
	if failing != nil {
		sent.forgetFailures(failureKeyPrefix, failing)
	}
	housekeeping := len(sent.Records) != before

	fresh := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if !sent.quiet(c.Key, now) {
			fresh = append(fresh, c)
		}
	}
	plan := planMessages(tenantID, settings, fresh, sent, now)
	if len(plan) == 0 {
		if housekeeping {
			// Losing this write costs nothing: it is cleanup, redone next round.
			_ = e.store.SaveSent(tenantID, sent, version)
		}
		return
	}

	for _, p := range plan {
		for _, c := range p.cands {
			sent.set(c.Key, SentRecord{At: now, Until: now.Add(claimLease), Pending: true})
		}
		sent.set(gapKey(p.rule), SentRecord{At: now, Until: now.Add(minMessageGap)})
	}
	if err := e.store.SaveSent(tenantID, sent, version); err != nil {
		if !errors.Is(err, ErrConflict) {
			log.Warnf("alerting: could not reserve alerts for tenant %s: %v", tenantID, err)
		}
		return
	}

	channels := settings.ActiveChannels()
	trusted := TrustedTenant(tenantID)
	events := make([]Event, 0, len(plan))
	for i := range plan {
		if ctx.Err() != nil {
			plan[i].skipped = true
			continue
		}
		deliveries := e.sender.Send(ctx, channels, plan[i].msg, trusted)
		events = append(events, eventFor(plan[i].msg, deliveries))
		for _, d := range deliveries {
			if d.OK() {
				plan[i].ok = true
			} else {
				// The reason is safe to log (see sendError); the address and token never are.
				log.Warnf("alerting: delivery to a %s channel of tenant %s failed: %s", d.Kind, tenantID, d.Error)
			}
		}
	}
	e.settle(tenantID, plan, time.Duration(settings.CooldownMinutes)*time.Minute, e.now())
	if len(events) > 0 {
		if err := e.store.AppendEvents(tenantID, events); err != nil {
			log.Warnf("alerting: could not record the alert history of tenant %s: %v", tenantID, err)
		}
	}
}

// settle replaces the reservations with what became of the messages.
func (e *Engine) settle(tenantID string, plan []plannedMessage, cooldown time.Duration, now time.Time) {
	for attempt := 1; attempt <= 3; attempt++ {
		sent, version, err := e.store.LoadSent(tenantID)
		if err != nil {
			log.Warnf("alerting: could not settle the alerts of tenant %s: %v", tenantID, err)
			return
		}
		for _, p := range plan {
			for _, c := range p.cands {
				switch {
				case p.skipped:
					delete(sent.Records, c.Key)
				case p.ok:
					sent.set(c.Key, SentRecord{At: now, Until: quietUntil(c, cooldown, now)})
				default:
					sent.set(c.Key, SentRecord{At: now, Until: now.Add(failedRetryAfter), Failed: true})
				}
			}
		}
		sent.prune(now)
		err = e.store.SaveSent(tenantID, sent, version)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrConflict) {
			log.Warnf("alerting: could not settle the alerts of tenant %s: %v", tenantID, err)
			return
		}
		retrySleep(attempt)
	}
	// The reservations expire on their own, after which the problems are reported
	// again. A duplicate message is the price of losing this write, never a
	// missing one.
	log.Warnf("alerting: gave up settling the alerts of tenant %s after repeated conflicts", tenantID)
}

// quietUntil is when a reported problem may be reported again: after the
// window it belongs to renews when the rule knows that time, after one cooldown
// otherwise.
func quietUntil(c Candidate, cooldown time.Duration, now time.Time) time.Time {
	if !c.Until.IsZero() && c.Until.After(now) {
		return c.Until.Add(resetGrace)
	}
	return now.Add(cooldown)
}

var severityRank = map[string]int{SeverityCritical: 3, SeverityWarning: 2, SeverityInfo: 1}

func maxSeverity(a, b string) string {
	if severityRank[b] > severityRank[a] {
		return b
	}
	return a
}

// planMessages turns candidates into the messages of this round: a rule that
// found a few problems sends one message each, a rule that found many sends one
// summary. A rule that sent something within the last minMessageGap is skipped
// and its candidates wait for the next round.
func planMessages(tenantID string, settings Settings, fresh []Candidate, sent SentState, now time.Time) []plannedMessage {
	byRule := make(map[string][]Candidate, len(ruleOrder))
	for _, c := range fresh {
		byRule[c.Rule] = append(byRule[c.Rule], c)
	}
	var plan []plannedMessage
	for _, rule := range ruleOrder {
		cands := byRule[rule]
		if len(cands) == 0 || sent.quiet(gapKey(rule), now) {
			continue
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if ri, rj := severityRank[cands[i].Severity], severityRank[cands[j].Severity]; ri != rj {
				return ri > rj
			}
			if cands[i].Subject.Name != cands[j].Subject.Name {
				return cands[i].Subject.Name < cands[j].Subject.Name
			}
			return cands[i].Key < cands[j].Key
		})
		if len(cands) > maxItems {
			cands = cands[:maxItems]
		}
		if len(cands) <= groupAbove {
			for _, c := range cands {
				plan = append(plan, plannedMessage{rule: rule, cands: []Candidate{c}, msg: newMessage(tenantID, rule, settings.Language, now, []Candidate{c})})
			}
			continue
		}
		plan = append(plan, plannedMessage{rule: rule, cands: cands, msg: newMessage(tenantID, rule, settings.Language, now, cands)})
	}
	return plan
}

// newMessage writes the message for one or several candidates of a rule.
func newMessage(tenantID, rule, language string, now time.Time, cands []Candidate) Message {
	items := make([]Item, 0, len(cands))
	severity := SeverityInfo
	for _, c := range cands {
		items = append(items, Item{
			Subject: Subject{Kind: c.Subject.Kind, ID: cleanText(c.Subject.ID), Name: cleanText(c.Subject.Name)},
			Facts:   cleanFacts(c.Facts),
			Text:    cleanText(c.Describe(language)),
		})
		severity = maxSeverity(severity, c.Severity)
	}
	msg := Message{
		Version: payloadVersion, ID: uuid.NewString(), Time: now.UTC(), TenantID: tenantID,
		Rule: rule, Severity: severity, Language: language, Items: items,
	}
	if len(items) == 1 {
		msg.Title = ruleTitle(rule, language, 1)
		msg.Subject = items[0].Subject
		msg.Text = items[0].Text
		return msg
	}
	msg.Title = fmt.Sprintf("%s (%d)", ruleTitle(rule, language, len(items)), len(items))
	msg.Subject = Subject{Kind: items[0].Subject.Kind, Name: groupSubjectName(items[0].Subject.Kind, len(items), language)}
	lines := make([]string, 0, summaryLines+1)
	for i, item := range items {
		if i == summaryLines {
			lines = append(lines, moreLine(len(items)-summaryLines, language))
			break
		}
		lines = append(lines, "• "+item.Text)
	}
	msg.Text = strings.Join(lines, "\n")
	return msg
}
