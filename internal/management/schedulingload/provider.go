// Package schedulingload feeds observed upstream quota usage into credential
// scheduling.
//
// The selector package deliberately knows nothing about storage, so this
// package adapts the persisted AI account status (which already carries the
// per-window quota percentages collected by the account status probes) into the
// coreauth.QuotaLoadSource contract.
package schedulingload

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// snapshotTTL bounds how stale a quota reading may be before a refresh is
// triggered. Quota percentages move on the order of minutes, and the account
// status probes themselves run far less often than request selection, so a
// short TTL here costs nothing and keeps the data fresh enough to steer traffic.
const snapshotTTL = 60 * time.Second

// window is one quota window as the snapshot keeps it.
type window struct {
	key  string
	span time.Duration
	// used is the share already spent, in [0,1]. The status records store the
	// share *remaining*, which is what the panel shows.
	used float64
	// resetAt is zero when the provider gave no renewal time.
	resetAt time.Time
}

type snapshot struct {
	windows     map[string][]window
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
	p.current.Store(&snapshot{windows: map[string][]window{}})
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

// QuotaLoadRatio implements coreauth.QuotaLoadSource: the account-wide windows'
// load, for callers that do not know the request's model.
func (p *Provider) QuotaLoadRatio(auth *coreauth.Auth) (float64, bool) {
	return p.QuotaLoadRatioForModel(auth, "")
}

// QuotaLoadRatioForModel implements coreauth.ModelQuotaLoadSource: the share
// spent of the most constrained window that limits model, with long windows
// close to renewal discounted (see longWindowDiscount).
func (p *Provider) QuotaLoadRatioForModel(auth *coreauth.Auth, model string) (float64, bool) {
	windows, ok := p.windowsFor(auth)
	if !ok {
		return 0, false
	}
	now := time.Now()
	worst, found := 0.0, false
	for _, w := range windows {
		if !aiaccountstatus.WindowAppliesToModel(w.key, model) {
			continue
		}
		w = w.at(now)
		if ratio := w.used * longWindowDiscount(w, now); !found || ratio > worst {
			worst, found = ratio, true
		}
	}
	return worst, found
}

// QuotaWindows implements coreauth.QuotaWindowSource for the reset-aware
// distribution.
func (p *Provider) QuotaWindows(auth *coreauth.Auth, model string) ([]coreauth.QuotaWindow, bool) {
	windows, ok := p.windowsFor(auth)
	if !ok {
		return nil, false
	}
	now := time.Now()
	out := make([]coreauth.QuotaWindow, 0, len(windows))
	for _, w := range windows {
		if !aiaccountstatus.WindowAppliesToModel(w.key, model) {
			continue
		}
		w = w.at(now)
		resetAt := w.resetAt
		if resetAt.IsZero() && w.span > 0 {
			// Not started, or renewed since it was read: the next request starts
			// it, so it renews a full span from now.
			resetAt = now.Add(w.span)
		}
		out = append(out, coreauth.QuotaWindow{Key: w.key, Span: w.span, Used: w.used, ResetAt: resetAt})
	}
	return out, len(out) > 0
}

func (p *Provider) windowsFor(auth *coreauth.Auth) ([]window, bool) {
	if p == nil || auth == nil {
		return nil, false
	}
	key := loadKey(auth.TenantID, auth.Index)
	if key == "" {
		return nil, false
	}
	current := p.current.Load()
	if current == nil {
		p.triggerRefresh()
		return nil, false
	}
	if time.Since(current.refreshedAt) > snapshotTTL {
		p.triggerRefresh()
	}
	windows, ok := current.windows[key]
	return windows, ok && len(windows) > 0
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

	byAccount := make(map[string][]window)
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
			if windows := windowsFromStatus(record.Quotas); len(windows) > 0 {
				byAccount[key] = windows
			}
		}
	}
	p.current.Store(&snapshot{windows: byAccount, refreshedAt: time.Now()})
}

// windowsFromStatus keeps the windows whose usage is known. A window without a
// share says nothing about how much room the account has.
func windowsFromStatus(dtos []usage.QuotaWindowDTO) []window {
	out := make([]window, 0, len(dtos))
	for _, dto := range dtos {
		if dto.Percent == nil {
			continue
		}
		remaining := math.Min(math.Max(*dto.Percent, 0), 100)
		w := window{
			key:  dto.QuotaKey,
			span: time.Duration(dto.WindowSeconds) * time.Second,
			used: 1 - remaining/100,
		}
		if dto.ResetAt != nil {
			w.resetAt = dto.ResetAt.UTC()
		}
		out = append(out, w)
	}
	return out
}

// at returns the window as it stands at now. A window whose renewal has passed
// since it was read has started over: nothing of it is spent, and when it
// renews next is unknown until the provider reports it.
func (w window) at(now time.Time) window {
	if !w.resetAt.IsZero() && !w.resetAt.After(now) {
		w.used = 0
		w.resetAt = time.Time{}
	}
	return w
}

// Reset-discount tuning for long (weekly or longer) windows. Within
// fullBurnWindow of a renewal what such a window has left is about to be wiped,
// so the account should burn it rather than shed load. From fullBurnWindow out
// to discountHorizon the discount fades linearly back to none, so an account
// whose renewal is days away is protected as usual.
//
// Shorter windows are never discounted: a five-hour window always renews within
// fullBurnWindow, and running it into its cap means 429s until it does.
const (
	longWindowSpan  = 7 * 24 * time.Hour
	fullBurnWindow  = 6 * time.Hour
	discountHorizon = 48 * time.Hour
)

// longWindowDiscount scales a window's used share as its renewal approaches. It
// returns 1 for short windows and for windows without a known renewal.
func longWindowDiscount(w window, now time.Time) float64 {
	if w.span < longWindowSpan || w.resetAt.IsZero() {
		return 1
	}
	until := w.resetAt.Sub(now)
	if until <= fullBurnWindow {
		return 0
	}
	if until >= discountHorizon {
		return 1
	}
	return float64(until-fullBurnWindow) / float64(discountHorizon-fullBurnWindow)
}

func loadKey(tenantID, authIndex string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	return coreauth.NormalizedTenantID(tenantID) + "|" + authIndex
}
