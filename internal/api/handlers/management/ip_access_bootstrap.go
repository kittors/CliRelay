package management

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// ensureIPAccessRuntime binds the allow/deny list and the attempt log to
// whatever database the runtime currently holds.
//
// Both helpers are idempotent and re-bind when the pool is swapped, so calling
// this from handler construction and from every config reload is cheap and
// keeps the registry pointing at a live handle.
//
// Policy persistence is not wired here: it lives in the settings store, which
// management handlers deliberately do not reach into. The CLI runner attaches
// the persister on top. Without it the protection policy still applies, it just
// resets to defaults on restart — a far better failure mode than the endpoints
// answering 503.
func ensureIPAccessRuntime(cfg *config.Config) {
	db := usage.RuntimeDB()
	if db == nil {
		return
	}
	ctx := context.Background()
	authevents.EnsureDefault(ctx, db)

	var trustedProxies []string
	if cfg != nil {
		trustedProxies = cfg.TrustedProxies
	}
	registry := ipaccess.EnsureDefault(ctx, db, trustedProxies)
	// Protection follows the list actually in force, which may come from the
	// database rather than config.yaml.
	effectiveProxies, _ := registry.TrustedProxies()
	registry.SetProtectedAddresses(
		ipaccess.LocalInterfaceAddresses(),
		effectiveProxies,
		ipaccess.ProxyHostAddresses(outboundProxyURLs(cfg)),
	)
}

// outboundProxyURLs collects the egress proxy pool so those hosts are protected
// too. Operators reasonably read "our proxies" as something that must never be
// blocked; an egress proxy never arrives as an inbound client, so protecting it
// costs nothing and stops a confusing manual rule from being written instead.
func outboundProxyURLs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	urls := make([]string, 0, len(cfg.ProxyPool))
	for _, entry := range cfg.ProxyPool {
		if strings.TrimSpace(entry.URL) != "" {
			urls = append(urls, entry.URL)
		}
	}
	return urls
}
