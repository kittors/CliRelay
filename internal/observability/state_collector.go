package observability

import (
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Account states as exported. Every state is reported for every provider that
// has an account, zeros included, so an alert on cooling accounts does not have
// to treat a missing series as zero.
const (
	accountStateActive   = "active"
	accountStateCooling  = "cooling"
	accountStateError    = "error"
	accountStateDisabled = "disabled"
)

var accountStates = []string{accountStateActive, accountStateCooling, accountStateError, accountStateDisabled}

// stateCollector reads the state that is already held in memory at scrape time
// rather than tracking it through events: an account's cooldown ends by the
// clock, with no event to count.
type stateCollector struct {
	exporter *Exporter

	inFlight       *prometheus.Desc
	accounts       *prometheus.Desc
	modelCooldowns *prometheus.Desc
}

func newStateCollector(e *Exporter) *stateCollector {
	return &stateCollector{
		exporter: e,
		inFlight: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "http", "inflight_requests"),
			"HTTP requests being served right now, streams included.",
			nil, nil),
		accounts: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "accounts"),
			"AI accounts by state. cooling: the whole account is held back until a reset time; error: the last refresh or request left it in error; disabled: switched off by an operator.",
			[]string{"provider", "state"}, nil),
		modelCooldowns: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "account_model_cooldowns"),
			"Models held back on accounts that are otherwise usable, by the reason the account list shows.",
			[]string{"provider", "reason"}, nil),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.inFlight
	ch <- c.accounts
	ch <- c.modelCooldowns
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	sources := c.exporter.currentSources()
	if sources.InFlight != nil {
		ch <- prometheus.MustNewConstMetric(c.inFlight, prometheus.GaugeValue, float64(sources.InFlight()))
	}
	if sources.Auths == nil {
		return
	}
	now := c.exporter.now()
	states := map[string]map[string]int{}
	cooldowns := map[[2]string]int{}
	for _, auth := range sources.Auths() {
		if auth == nil {
			continue
		}
		provider := labelValue(auth.Provider, unknownLabel)
		if states[provider] == nil {
			states[provider] = map[string]int{}
		}
		state := accountState(auth, now)
		states[provider][state]++
		// A held-back account's model states say nothing more; counting them
		// would report the same outage twice.
		if state != accountStateActive {
			continue
		}
		for _, cooldown := range coreauth.ActiveModelCooldowns(auth, now) {
			cooldowns[[2]string{provider, labelValue(cooldown.Reason, unknownLabel)}]++
		}
	}

	providers := make([]string, 0, len(states))
	for provider := range states {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		for _, state := range accountStates {
			ch <- prometheus.MustNewConstMetric(c.accounts, prometheus.GaugeValue, float64(states[provider][state]), provider, state)
		}
	}
	for key, count := range cooldowns {
		ch <- prometheus.MustNewConstMetric(c.modelCooldowns, prometheus.GaugeValue, float64(count), key[0], key[1])
	}
}

// accountState classifies a credential the way the scheduler treats it as a
// whole. Antigravity keeps separate quota pools per model family, so its
// account-level quota flag does not hold the whole account back; its exhausted
// pools show up as model cooldowns instead.
func accountState(auth *coreauth.Auth, now time.Time) string {
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return accountStateDisabled
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) && !coreauth.IsAntigravityAuth(auth) {
		return accountStateCooling
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return accountStateCooling
	}
	if auth.Status == coreauth.StatusError {
		return accountStateError
	}
	return accountStateActive
}
