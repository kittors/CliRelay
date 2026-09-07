// Package schedulingload feeds observed upstream quota usage into credential
// scheduling.
//
// The selector package deliberately knows nothing about storage, so this
// package adapts the persisted AI account status (which already carries the
// per-window quota percentages collected by the account status probes) into the
// coreauth.QuotaLoadSource contract.
package schedulingload

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// snapshotTTL bounds how stale a quota reading may be before a refresh is
// triggered. Quota percentages move on the order of minutes, and the account
// status probes themselves run far less often than request selection, so a
// short TTL here costs nothing and keeps the data fresh enough to steer traffic.
const snapshotTTL = 60 * time.Second

type snapshot struct {
	ratios      map[string]float64
	refreshedAt time.Time
}

// Provider serves cached quota load ratios and refreshes them out of band.
//
// Reads never block on the database: a stale snapshot is returned while a
// refresh runs in the background, and an empty snapshot simply reports "no
// data" so scheduling falls back to its in-process signals.
type Provider struct {
	current    atomic.Pointer[snapshot]
	refreshing atomic.Bool
	tenantsMu  sync.RWMutex
	tenants    map[string]struct{}
}

func NewProvider() *Provider {
	p := &Provider{tenants: make(map[string]struct{})}
	p.current.Store(&snapshot{ratios: map[string]float64{}})
	return p
}

// TrackTenant registers a tenant whose accounts should be included in refreshes.
func (p *Provider) TrackTenant(tenantID string) {
	tenantID = strings.TrimSpace(tenantID)
	if p == nil || tenantID == "" {
		return
	}
	p.tenantsMu.Lock()
	p.tenants[tenantID] = struct{}{}
	p.tenantsMu.Unlock()
}

// QuotaLoadRatio implements coreauth.QuotaLoadSource.
func (p *Provider) QuotaLoadRatio(auth *coreauth.Auth) (float64, bool) {
	if p == nil || auth == nil {
		return 0, false
	}
	key := loadKey(auth.TenantID, auth.Index)
	if key == "" {
		return 0, false
	}

	current := p.current.Load()
	if current == nil {
		p.triggerRefresh()
		return 0, false
	}
	if time.Since(current.refreshedAt) > snapshotTTL {
		p.triggerRefresh()
	}
	ratio, ok := current.ratios[key]
	return ratio, ok
}

func (p *Provider) triggerRefresh() {
	if !p.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.refreshing.Store(false)
		p.Refresh()
	}()
}

// Refresh rebuilds the snapshot synchronously. Exported for startup priming and
// for tests that need deterministic timing.
func (p *Provider) Refresh() {
	if p == nil {
		return
	}
	p.tenantsMu.RLock()
	tenants := make([]string, 0, len(p.tenants))
	for tenantID := range p.tenants {
		tenants = append(tenants, tenantID)
	}
	p.tenantsMu.RUnlock()

	ratios := make(map[string]float64)
	for _, tenantID := range tenants {
		records, err := usage.ListAIAccountStatusForTenant(tenantID, nil)
		if err != nil {
			// A missing or unreachable store must not break scheduling; the
			// selectors fall back to their in-process load signals.
			log.Debugf("schedulingload: list account status for tenant %s: %v", tenantID, err)
			continue
		}
		for _, record := range records {
			key := loadKey(tenantID, record.AuthIndex)
			if key == "" {
				continue
			}
			if ratio, ok := worstQuotaRatio(record.Quotas); ok {
				ratios[key] = ratio
			}
		}
	}
	p.current.Store(&snapshot{ratios: ratios, refreshedAt: time.Now()})
}

// worstQuotaRatio takes the most constrained window for the account. An account
// whose 5-hour window is nearly spent should shed traffic even when its weekly
// window still looks healthy.
func worstQuotaRatio(windows []usage.QuotaWindowDTO) (float64, bool) {
	worst := 0.0
	found := false
	for _, window := range windows {
		if window.Percent == nil {
			continue
		}
		ratio := *window.Percent / 100
		if ratio < 0 {
			continue
		}
		if ratio > 1 {
			ratio = 1
		}
		if !found || ratio > worst {
			worst = ratio
			found = true
		}
	}
	return worst, found
}

func loadKey(tenantID, authIndex string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	return coreauth.NormalizedTenantID(tenantID) + "|" + authIndex
}
