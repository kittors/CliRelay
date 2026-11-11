package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// errEgressAccountsUnavailable is returned while there is no auth manager to
// read the accounts from. Answering "no proxies" instead would make every
// outage reported earlier look like a proxy nobody uses any more.
var errEgressAccountsUnavailable = errors.New("the accounts are not loaded yet")

// egressKindRank decides what a proxy is called when it is reached in several
// ways at once (an entry, and a proxy-url naming the same address): by the
// entry the operator sees in the console first.
var egressKindRank = map[string]int{
	alerting.EgressPoolEntry:    4,
	alerting.EgressSharedEntry:  3,
	alerting.EgressAccountProxy: 2,
	alerting.EgressDefaultProxy: 1,
}

// Egress returns the proxies the tenant's traffic leaves through, with what
// the egress breaker of this process knows about each.
//
// They are the tenant's own enabled pool entries, used or not (the system
// tenant's include those it shares, which other tenants' accounts use), and
// every proxy one of its switched-on accounts leaves through: a shared entry,
// a proxy-url set on the account, the default proxy-url. Each account's proxy
// is asked of its executor, which resolves it as the requests do, so the
// address is the key the breaker keeps its state under. Like the rest of
// alertSources it reads memory and stored settings only.
func (s alertSources) Egress(_ context.Context, tenantID string, _ time.Time) ([]alerting.EgressSample, error) {
	var manager *coreauth.Manager
	if s.authManager != nil {
		manager = s.authManager()
	}
	if manager == nil {
		return nil, errEgressAccountsUnavailable
	}
	var own, shared []config.ProxyPoolEntry
	if s.proxyPools != nil {
		own, shared = s.proxyPools(tenantID)
	}
	pool := config.MergeSharedProxyPool(own, shared)
	ownIDs := make(map[string]struct{}, len(own))
	for _, entry := range own {
		ownIDs[config.NormalizeProxyID(entry.ID)] = struct{}{}
	}

	byURL := make(map[string]*alerting.EgressSample)
	note := func(url, kind, id, name string) *alerting.EgressSample {
		sample := byURL[url]
		if sample == nil {
			sample = &alerting.EgressSample{Endpoint: maskProxyPoolURL(url), Fingerprint: egressFingerprint(url)}
			byURL[url] = sample
		}
		if egressKindRank[kind] > egressKindRank[sample.Kind] {
			sample.Kind, sample.ID, sample.Name = kind, id, name
		}
		return sample
	}
	for _, entry := range own {
		if url := strings.TrimSpace(entry.URL); entry.Enabled && url != "" {
			note(url, alerting.EgressPoolEntry, config.NormalizeProxyID(entry.ID), entry.Name)
		}
	}
	for _, auth := range manager.ListForTenant(tenantID) {
		// Accounts the operator switched off send nothing.
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		url := manager.EgressProxyURL(auth)
		if url == "" {
			continue
		}
		kind, id, name := egressKindOf(auth, url, pool, ownIDs)
		note(url, kind, id, name).Accounts++
	}

	breaker := s.breaker
	if breaker == nil {
		breaker = egressbreaker.Default()
	}
	out := make([]alerting.EgressSample, 0, len(byURL))
	for url, sample := range byURL {
		observation := breaker.Observe(url)
		sample.LastPass = observation.LastPass
		if observation.Open {
			sample.Open = true
			sample.Fault = observation.Fault.Code()
			sample.Since = observation.Since
			sample.NextProbe = observation.NextProbe
			sample.Refused = observation.Refused
			sample.FailedProbes = observation.FailedProbes
		}
		out = append(out, *sample)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := egressKindRank[out[i].Kind], egressKindRank[out[j].Kind]; ri != rj {
			return ri > rj
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}

// egressKindOf says where the proxy an account leaves through is configured,
// following config.ResolveProxyURL: the enabled pool entry the account names,
// else the proxy-url set on the account, else the default one.
func egressKindOf(auth *coreauth.Auth, url string, pool []config.ProxyPoolEntry, ownIDs map[string]struct{}) (kind, id, name string) {
	if want := config.NormalizeProxyID(auth.ProxyID); want != "" {
		for _, entry := range pool {
			if !entry.Enabled || config.NormalizeProxyID(entry.ID) != want || strings.TrimSpace(entry.URL) != url {
				continue
			}
			if _, isOwn := ownIDs[want]; isOwn {
				return alerting.EgressPoolEntry, want, entry.Name
			}
			return alerting.EgressSharedEntry, want, entry.Name
		}
	}
	if strings.TrimSpace(auth.ProxyURL) == url {
		return alerting.EgressAccountProxy, "", ""
	}
	return alerting.EgressDefaultProxy, "", ""
}

// egressFingerprint stands for a proxy address, credentials included, in the
// alert bookkeeping: two addresses on one host with different credentials are
// different proxies. It never leaves the server.
func egressFingerprint(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:8])
}
