package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

type ProxyHealthStatus struct {
	ProxyID             string    `json:"proxy_id"`
	Healthy             bool      `json:"healthy"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastCheckAt         time.Time `json:"last_check_at"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastLatencyMs       int64     `json:"last_latency_ms,omitempty"`
	AutoDisabledAt      time.Time `json:"auto_disabled_at,omitempty"`
	TotalChecks         int64     `json:"total_checks"`
	TotalFailures       int64     `json:"total_failures"`
}

type HealthChecker struct {
	mu       sync.RWMutex
	cfg      config.ProxyHealthConfig
	statuses map[string]*ProxyHealthStatus
	sdkCfg   *config.SDKConfig
}

func NewHealthChecker(cfg config.ProxyHealthConfig, sdkCfg *config.SDKConfig) *HealthChecker {
	return &HealthChecker{
		cfg:      cfg,
		statuses: make(map[string]*ProxyHealthStatus),
		sdkCfg:   sdkCfg,
	}
}

func (h *HealthChecker) CheckProxy(ctx context.Context, entry config.ProxyPoolEntry) *ProxyHealthStatus {
	start := time.Now()
	ok, latencyMs, err := h.checkConnectivity(ctx, entry.URL)

	h.mu.Lock()
	defer h.mu.Unlock()

	status, exists := h.statuses[entry.ID]
	if !exists {
		status = &ProxyHealthStatus{
			ProxyID: entry.ID,
			Healthy: true,
		}
		h.statuses[entry.ID] = status
	}

	now := time.Now()
	status.LastCheckAt = now
	status.TotalChecks++

	if ok {
		status.ConsecutiveFailures = 0
		status.LastSuccessAt = now
		status.LastLatencyMs = latencyMs
		status.Healthy = true

		if !status.AutoDisabledAt.IsZero() {
			log.Infof("proxy health: proxy %s recovered after being auto-disabled", entry.ID)
			status.AutoDisabledAt = time.Time{}
		}
	} else {
		status.ConsecutiveFailures++
		status.LastFailureAt = now
		status.TotalFailures++
		status.LastLatencyMs = time.Since(start).Milliseconds()

		if status.ConsecutiveFailures >= h.cfg.FailureThreshold {
			if status.Healthy {
				log.Warnf("proxy health: proxy %s marked unhealthy after %d consecutive failures (last error: %v)", entry.ID, status.ConsecutiveFailures, err)
			}
			status.Healthy = false
		}
	}

	return status
}

func (h *HealthChecker) checkConnectivity(ctx context.Context, proxyURL string) (bool, int64, error) {
	transport := buildProxyTransport(proxyURL, h.sdkCfg != nil && h.sdkCfg.PreferIPv4)
	if transport == nil {
		return false, 0, fmt.Errorf("failed to build proxy transport")
	}
	if h.sdkCfg != nil && h.sdkCfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		} else {
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
	}

	client := &http.Client{
		Timeout:   h.cfg.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.TestURL, nil)
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("User-Agent", "CLIProxyAPI/proxy-health-checker")

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return false, latency, err
	}
	defer resp.Body.Close()

	return resp.StatusCode < 500, latency, nil
}

func (h *HealthChecker) GetStatus(proxyID string) *ProxyHealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status, ok := h.statuses[proxyID]
	if !ok {
		return &ProxyHealthStatus{ProxyID: proxyID, Healthy: true}
	}
	cp := *status
	return &cp
}

func (h *HealthChecker) GetAllStatuses() map[string]*ProxyHealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make(map[string]*ProxyHealthStatus, len(h.statuses))
	for k, v := range h.statuses {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (h *HealthChecker) MarkAutoDisabled(proxyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if status, ok := h.statuses[proxyID]; ok {
		status.AutoDisabledAt = time.Now()
	}
}

func (h *HealthChecker) ShouldRecover(proxyID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status, ok := h.statuses[proxyID]
	if !ok {
		return false
	}
	if status.AutoDisabledAt.IsZero() {
		return false
	}
	if status.Healthy {
		return true
	}
	return time.Since(status.AutoDisabledAt) >= h.cfg.RecoveryInterval
}

func (h *HealthChecker) ResetAfterRecovery(proxyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if status, ok := h.statuses[proxyID]; ok {
		status.AutoDisabledAt = time.Time{}
		status.Healthy = true
		status.ConsecutiveFailures = 0
	}
}

func (h *HealthChecker) CheckAll(ctx context.Context, pool []config.ProxyPoolEntry) []string {
	disabled := make([]string, 0)
	for _, entry := range pool {
		if !entry.Enabled {
			continue
		}
		status := h.CheckProxy(ctx, entry)

		if !status.Healthy && status.AutoDisabledAt.IsZero() && status.ConsecutiveFailures >= h.cfg.FailureThreshold {
			h.MarkAutoDisabled(entry.ID)
			disabled = append(disabled, entry.ID)
			log.Warnf("proxy health: auto-disabling proxy %s after %d consecutive failures", entry.ID, status.ConsecutiveFailures)
		}
	}
	return disabled
}

func (h *HealthChecker) RecoverDisabled(ctx context.Context, pool []config.ProxyPoolEntry) []string {
	recovered := make([]string, 0)
	for _, entry := range pool {
		if !entry.Enabled {
			continue
		}
		if h.ShouldRecover(entry.ID) {
			status := h.CheckProxy(ctx, entry)
			if status.Healthy {
				h.ResetAfterRecovery(entry.ID)
				recovered = append(recovered, entry.ID)
				log.Infof("proxy health: proxy %s recovered and re-enabled", entry.ID)
			}
		}
	}
	return recovered
}

func (h *HealthChecker) Interval() time.Duration {
	if h.cfg.Interval <= 0 {
		return 5 * time.Minute
	}
	return h.cfg.Interval
}

func buildProxyTransport(proxyURL string, preferIPv4 bool) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return buildDefaultTransport(preferIPv4)
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return buildDefaultTransport(preferIPv4)
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if preferIPv4 {
		dialer.FallbackDelay = -1
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		Proxy:                http.ProxyURL(parsed),
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		TLSClientConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return transport
}

func buildDefaultTransport(preferIPv4 bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if preferIPv4 {
		dialer.FallbackDelay = -1
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		TLSClientConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
