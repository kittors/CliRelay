package ipaccess

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

const (
	// refreshInterval bounds how long a rule change takes to reach another
	// process. The deployment runs blue/green slots that share no memory, so the
	// database is the only common ground and every slot must poll it.
	refreshInterval = 15 * time.Second
	// hitFlushInterval batches match counters back to the database.
	hitFlushInterval = time.Minute
	// autoPurgeInterval reclaims lapsed automatic bans.
	autoPurgeInterval = time.Hour
)

// Verdict is the admission decision for one request.
type Verdict struct {
	Decision Decision
	// Rule is the rule that matched, nil for lockdown rejections and passes.
	Rule *Rule
	// Enforced is false when the list was consulted but could not be applied,
	// which happens when the client address is not trustworthy. Callers must
	// treat a non-enforced verdict as "allow and warn", never as "deny".
	Enforced bool
	// Reason explains a non-enforced verdict for diagnostics.
	Reason string
}

// Allowed reports whether the request may proceed.
func (v Verdict) Allowed() bool {
	return !v.Enforced || v.Decision != DecisionDeny
}

// Exempt reports whether the request matched an allow rule and should therefore
// skip rate limiting.
func (v Verdict) Exempt() bool {
	return v.Enforced && v.Decision == DecisionAllow
}

const (
	reasonUntrustedClientIP = "client address is not trustworthy (relayed request with no trusted-proxies)"
	reasonNoStorage         = "rule storage unavailable"
)

// Registry owns the live rule snapshot and keeps it in step with the database.
type Registry struct {
	store     *Store
	matcher   atomic.Pointer[Matcher]
	policy    atomic.Pointer[ProtectionPolicy]
	persister PolicyPersister

	// protected is the set of addresses no rule may reject.
	protected atomic.Pointer[protectedSet]

	// configProxies is the config.yaml fallback, used only when the stored policy
	// lists none. Kept separately so saving an empty list in the panel restores
	// the configured value instead of stranding the deployment with nothing.
	configProxies atomic.Pointer[[]string]
	// proxyMatcher resolves the forwarding chain. It is rebuilt whenever the
	// policy or the config changes, so an operator can fix trust from the panel
	// and have it apply to the very next request.
	proxyMatcher atomic.Pointer[trustedProxyMatcher]

	hitsMu sync.Mutex
	hits   map[string]int64

	stop     chan struct{}
	stopOnce sync.Once

	autoBan *autoBanEngine
	alerter *alerter
}

// NewRegistry builds a registry over the given store. The snapshot starts empty,
// so requests pass until the first refresh completes — failing open on startup
// is deliberate: a database hiccup must not make the service unreachable.
func NewRegistry(store *Store) *Registry {
	r := &Registry{store: store, hits: make(map[string]int64), stop: make(chan struct{})}
	policy := DefaultPolicy()
	r.policy.Store(&policy)
	r.matcher.Store(NewMatcher(nil, false, time.Now()))
	r.autoBan = newAutoBanEngine(r)
	r.alerter = newAlerter()
	return r
}

// Policy returns the active protection policy.
func (r *Registry) Policy() ProtectionPolicy {
	if r == nil {
		return DefaultPolicy()
	}
	if p := r.policy.Load(); p != nil {
		return *p
	}
	return DefaultPolicy()
}

// SetPolicy swaps the policy in memory and rebuilds the snapshot, because the
// lockdown flag lives on the snapshot.
func (r *Registry) SetPolicy(ctx context.Context, policy ProtectionPolicy) {
	if r == nil {
		return
	}
	normalized := policy.Normalized()
	r.policy.Store(&normalized)
	r.rebuildProxyMatcher()
	if err := r.Refresh(ctx); err != nil {
		log.WithError(err).Warn("ip-access: refresh after policy change failed")
	}
}

// PolicyPersister stores the policy document.
//
// It is an interface so this package never learns about the settings store:
// the admission layer sits on the request path and must not acquire a
// dependency on the configuration subsystem to get there.
type PolicyPersister interface {
	Load() (ProtectionPolicy, bool)
	Save(ProtectionPolicy) error
}

// SetPolicyPersister attaches persistence and adopts whatever is already stored.
func (r *Registry) SetPolicyPersister(ctx context.Context, persister PolicyPersister) {
	if r == nil || persister == nil {
		return
	}
	r.persister = persister
	if stored, ok := persister.Load(); ok {
		r.SetPolicy(ctx, stored)
	}
}

// UpdatePolicy persists a new policy and applies it. The write happens first:
// a policy that took effect but did not survive a restart would silently revert,
// and an operator who saw "lockdown enabled" would be wrong after the next
// deploy.
func (r *Registry) UpdatePolicy(ctx context.Context, policy ProtectionPolicy) error {
	if r == nil {
		return nil
	}
	normalized := policy.Normalized()
	if r.persister != nil {
		if err := r.persister.Save(normalized); err != nil {
			return err
		}
	}
	r.SetPolicy(ctx, normalized)
	return nil
}

// ReloadPolicy re-adopts the stored policy. Used by the config watcher so a
// change written by the other blue/green slot is picked up here too.
func (r *Registry) ReloadPolicy(ctx context.Context) {
	if r == nil || r.persister == nil {
		return
	}
	if stored, ok := r.persister.Load(); ok {
		r.SetPolicy(ctx, stored)
	}
}

// SetProtectedAddresses rebuilds the un-bannable set.
func (r *Registry) SetProtectedAddresses(localAddresses, trustedProxies, outboundProxies []string) {
	if r == nil {
		return
	}
	r.protected.Store(buildProtectedSet(localAddresses, trustedProxies, outboundProxies))
}

// ProtectedEntries lists the un-bannable addresses for display.
func (r *Registry) ProtectedEntries() []ProtectedEntry {
	if r == nil {
		return nil
	}
	set := r.protected.Load()
	if set == nil {
		return nil
	}
	out := make([]ProtectedEntry, len(set.entries))
	copy(out, set.entries)
	return out
}

// Protected reports whether an address is structurally un-bannable.
func (r *Registry) Protected(ip net.IP) (ProtectedEntry, bool) {
	if r == nil {
		return ProtectedEntry{}, false
	}
	return r.protected.Load().match(ip)
}

// SetConfiguredProxies records the config.yaml fallback list.
func (r *Registry) SetConfiguredProxies(proxies []string) {
	if r == nil {
		return
	}
	copied := append([]string(nil), proxies...)
	r.configProxies.Store(&copied)
	r.rebuildProxyMatcher()
}

// TrustedProxies returns the list actually in force, and whether it came from
// the database or from config.yaml.
func (r *Registry) TrustedProxies() ([]string, string) {
	if r == nil {
		return nil, "none"
	}
	if stored := r.Policy().TrustedProxies; len(stored) > 0 {
		return append([]string(nil), stored...), "database"
	}
	if configured := r.configProxies.Load(); configured != nil && len(*configured) > 0 {
		return append([]string(nil), *configured...), "config"
	}
	return nil, "none"
}

func (r *Registry) rebuildProxyMatcher() {
	proxies, _ := r.TrustedProxies()
	r.proxyMatcher.Store(newTrustedProxyMatcher(proxies))
}

// ProxyTrusted reports whether any reverse proxy is declared, from either source.
func (r *Registry) ProxyTrusted() bool {
	if r == nil {
		return false
	}
	return r.proxyMatcher.Load().configured()
}

// ResolveAddress derives the client address for a request by walking the
// forwarding chain against the declared proxies, so admission and throttling can
// never disagree about whether an address is meaningful.
//
// resolvedIP (the framework's ClientIP) is accepted for compatibility but is no
// longer authoritative: it is fixed at engine construction from config.yaml,
// which is exactly the restart-to-change behaviour this replaces.
func (r *Registry) ResolveAddress(req *http.Request, resolvedIP string) ClientAddress {
	if r == nil {
		return Resolve(req, resolvedIP, false)
	}
	matcher := r.proxyMatcher.Load()
	if req == nil {
		return Resolve(req, resolvedIP, matcher.configured())
	}
	return resolveForwarded(req, matcher)
}

// Store exposes the rule store for the management API.
func (r *Registry) Store() *Store {
	if r == nil {
		return nil
	}
	return r.store
}

// Matcher exposes the current snapshot.
func (r *Registry) Matcher() *Matcher {
	if r == nil {
		return nil
	}
	return r.matcher.Load()
}

// Refresh rebuilds the snapshot from the database.
func (r *Registry) Refresh(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if !r.store.Available() {
		return nil
	}
	now := time.Now()
	rules, err := r.store.LoadActive(ctx, now)
	if err != nil {
		// The previous snapshot stays in force. Dropping every rule because one
		// query failed would silently unban whoever is currently being blocked.
		return err
	}
	r.matcher.Store(NewMatcher(rules, r.Policy().Lockdown, now))
	return nil
}

// Evaluate resolves a client address against the list.
func (r *Registry) Evaluate(addr ClientAddress) Verdict {
	if r == nil {
		return Verdict{Decision: DecisionNeutral}
	}
	// A genuine local connection is unconditionally admitted so a bad list can
	// never lock an operator out of the host the process runs on. Note this is
	// LocalOperator, not Loopback: a relayed chain that merely dead-ends on a
	// loopback address is an unresolved chain, and admitting it would exempt
	// every external client from the rules.
	if addr.LocalOperator() {
		return Verdict{Decision: DecisionAllow, Enforced: true, Reason: "local_operator"}
	}
	if !addr.Trusted {
		return Verdict{Decision: DecisionNeutral, Enforced: false, Reason: reasonUntrustedClientIP}
	}
	// Protected addresses win over every rule, including lockdown, and are checked
	// before storage availability because the guarantee is absolute: the host's own
	// address and its reverse proxy stay reachable whatever the rule store is doing.
	if entry, ok := r.protected.Load().match(addr.IP); ok {
		return Verdict{Decision: DecisionAllow, Enforced: true, Reason: string(entry.Reason)}
	}
	if !r.store.Available() {
		return Verdict{Decision: DecisionNeutral, Enforced: false, Reason: reasonNoStorage}
	}
	matcher := r.matcher.Load()
	decision, rule := matcher.Match(addr.IP)
	if rule != nil {
		r.noteHit(rule.ID)
	}
	return Verdict{Decision: decision, Rule: rule, Enforced: true}
}

func (r *Registry) noteHit(id string) {
	if id == "" {
		return
	}
	r.hitsMu.Lock()
	r.hits[id]++
	r.hitsMu.Unlock()
}

// AutoBan exposes the automatic ban engine.
func (r *Registry) AutoBan() *autoBanEngine {
	if r == nil {
		return nil
	}
	return r.autoBan
}

// Start launches the background loops. It returns immediately.
func (r *Registry) Start(ctx context.Context) {
	if r == nil || !r.store.Available() {
		return
	}
	if err := r.Refresh(ctx); err != nil {
		log.WithError(err).Warn("ip-access: initial rule load failed")
	}
	go r.loop(ctx)
}

func (r *Registry) loop(ctx context.Context) {
	refresh := time.NewTicker(refreshInterval)
	flush := time.NewTicker(hitFlushInterval)
	purge := time.NewTicker(autoPurgeInterval)
	defer refresh.Stop()
	defer flush.Stop()
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-refresh.C:
			if err := r.Refresh(ctx); err != nil {
				log.WithError(err).Debug("ip-access: periodic refresh failed")
			}
		case <-flush.C:
			r.flushHits(ctx)
		case <-purge.C:
			// Rules are shared; the cluster leader reclaims lapsed bans. Every
			// node still refreshes and flushes its own hit counters above.
			if !cluster.Default().IsLeader() {
				continue
			}
			// Keep lapsed bans for a day before reclaiming them so the panel can
			// still show what was recently blocked and why.
			if removed, err := r.store.PurgeExpiredAuto(ctx, time.Now().Add(-24*time.Hour)); err != nil {
				log.WithError(err).Debug("ip-access: purge expired auto rules failed")
			} else if removed > 0 {
				log.Debugf("ip-access: purged %d expired automatic rules", removed)
			}
		}
	}
}

func (r *Registry) flushHits(ctx context.Context) {
	r.hitsMu.Lock()
	if len(r.hits) == 0 {
		r.hitsMu.Unlock()
		return
	}
	pending := r.hits
	r.hits = make(map[string]int64)
	r.hitsMu.Unlock()
	if err := r.store.RecordHits(ctx, pending, time.Now()); err != nil {
		log.WithError(err).Debug("ip-access: flush rule hit counters failed")
	}
}

// Close stops the background loops. Safe to call more than once.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
}

var defaultRegistry atomic.Pointer[Registry]

// SetDefault publishes the process-wide registry.
func SetDefault(r *Registry) { defaultRegistry.Store(r) }

// Default returns the process-wide registry, or nil when none is configured.
func Default() *Registry { return defaultRegistry.Load() }
