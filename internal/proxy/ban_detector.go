package proxy

import (
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type BanEvent struct {
	AuthID    string    `json:"auth_id"`
	ProxyID   string    `json:"proxy_id"`
	Provider  string    `json:"provider"`
	Timestamp time.Time `json:"timestamp"`
	Reason    string    `json:"reason"`
}

type BanDetector struct {
	mu     sync.RWMutex
	cfg    config.ProxyBanDetectConfig
	events []BanEvent
}

func NewBanDetector(cfg config.ProxyBanDetectConfig) *BanDetector {
	return &BanDetector{
		cfg:    cfg,
		events: make([]BanEvent, 0),
	}
}

func (d *BanDetector) RecordBan(auth *coreauth.Auth, reason string) *BanEvent {
	if !d.cfg.Enabled || auth == nil {
		return nil
	}

	proxyID := auth.ProxyID
	if proxyID == "" {
		return nil
	}

	now := time.Now()
	event := BanEvent{
		AuthID:    auth.ID,
		ProxyID:   proxyID,
		Provider:  auth.Provider,
		Timestamp: now,
		Reason:    reason,
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.events = append(d.events, event)
	window := time.Duration(d.cfg.WindowMinutes) * time.Minute
	d.pruneEvents(now, window)

	log.Warnf("proxy ban detector: auth %s (provider=%s, proxy=%s) banned: %s", auth.ID, auth.Provider, proxyID, reason)

	return &event
}

func (d *BanDetector) CheckProxyBanned(proxyID string) (int, bool) {
	if !d.cfg.Enabled {
		return 0, false
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	now := time.Now()
	window := time.Duration(d.cfg.WindowMinutes) * time.Minute
	count := 0
	for _, e := range d.events {
		if e.ProxyID == proxyID && now.Sub(e.Timestamp) <= window {
			count++
		}
	}

	shouldDisable := d.cfg.AutoDisableProxy && count >= d.cfg.BanThreshold
	return count, shouldDisable
}

func (d *BanDetector) pruneEvents(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	writeIdx := 0
	for _, e := range d.events {
		if e.Timestamp.After(cutoff) {
			d.events[writeIdx] = e
			writeIdx++
		}
	}
	d.events = d.events[:writeIdx]
}

func (d *BanDetector) RecentEvents(proxyID string, limit int) []BanEvent {
	d.mu.RLock()
	defer d.mu.RUnlock()

	now := time.Now()
	window := time.Duration(d.cfg.WindowMinutes) * time.Minute
	out := make([]BanEvent, 0)
	for i := len(d.events) - 1; i >= 0 && len(out) < limit; i-- {
		e := d.events[i]
		if (proxyID == "" || e.ProxyID == proxyID) && now.Sub(e.Timestamp) <= window {
			out = append(out, e)
		}
	}
	return out
}

func (d *BanDetector) ProxyBanCounts() map[string]int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	now := time.Now()
	window := time.Duration(d.cfg.WindowMinutes) * time.Minute
	counts := make(map[string]int)
	for _, e := range d.events {
		if now.Sub(e.Timestamp) <= window {
			counts[e.ProxyID]++
		}
	}
	return counts
}
