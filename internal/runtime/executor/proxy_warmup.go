package executor

import (
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// ProxyWarmManager maintains idle connections from a fixed proxy to upstream AI API hosts.
// It sends lightweight, credential-free requests on a timer so that Go's http.Transport
// keeps TLS/HTTP2 connections warm, reducing first-token latency for real requests.
type ProxyWarmManager struct {
	cfg      config.ProxyWarmConfig
	proxyURL string
	sdkCfg   *config.SDKConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	hosts map[string]*warmHostState
}

type warmHostState struct {
	host             string
	lastUsed         time.Time
	consecutiveFails int
	nextWarmAt       time.Time
	targetURL        string
	targetMethod     string
}

// NewProxyWarmManager creates a warm manager. It is not started automatically—call Start().
func NewProxyWarmManager(cfg config.ProxyWarmConfig, proxyURL string, sdkCfg *config.SDKConfig) *ProxyWarmManager {
	m := &ProxyWarmManager{
		cfg:      cfg,
		proxyURL: proxyURL,
		sdkCfg:   sdkCfg,
		hosts:    make(map[string]*warmHostState),
	}
	m.seedConfiguredTargets()
	return m
}

// Start begins background connection warming. It blocks until the warmup loop is running
// or the context is cancelled. Call Stop() to shut down.
func (m *ProxyWarmManager) Start(parent context.Context) {
	if m == nil || !m.cfg.Enabled || m.proxyURL == "" {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	m.wg.Add(1)
	go m.run()
}

// Stop terminates the warmup loop and releases resources.
func (m *ProxyWarmManager) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
	m.wg.Wait()
}

// MarkUsed records that a real request just accessed the given host through the proxy.
// This ensures the host stays in the active warmup set.
func (m *ProxyWarmManager) MarkUsed(host string) {
	if m == nil || host == "" || !m.cfg.Enabled {
		return
	}
	host = normalizeWarmHost(host)
	if host == "" {
		return
	}
	if !m.isHostAllowed(host) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.hosts[host]
	if !exists {
		// Find target config for this host
		var targetURL, targetMethod string
		for _, t := range m.cfg.Targets {
			if strings.EqualFold(normalizeWarmHost(t.Host), host) {
				targetURL = t.URL
				targetMethod = t.Method
				break
			}
		}
		if targetURL == "" {
			// Dynamic host without a configured target — skip warming
			return
		}
		targetMethod = strings.ToUpper(strings.TrimSpace(targetMethod))
		if targetMethod == "" {
			targetMethod = http.MethodHead
		}
		m.hosts[host] = &warmHostState{
			host:         host,
			targetURL:    targetURL,
			targetMethod: targetMethod,
			lastUsed:     time.Now(),
			nextWarmAt:   time.Now(),
		}
		return
	}
	state.lastUsed = time.Now()
}

func (m *ProxyWarmManager) run() {
	defer m.wg.Done()

	startupDelay := warmDuration(m.cfg.StartupDelaySeconds)
	log.Debugf("proxy_warmup: starting in %v", startupDelay)

	select {
	case <-m.ctx.Done():
		return
	case <-time.After(startupDelay):
	}

	baseInterval := warmDuration(m.cfg.IntervalSeconds)
	inactiveTTL := time.Duration(m.cfg.InactiveTTLMinutes) * time.Minute
	timeout := warmDuration(m.cfg.TimeoutSeconds)

	var tickInterval time.Duration
	if baseInterval > 0 {
		tickInterval = baseInterval
	} else {
		tickInterval = 60 * time.Second
	}
	if tickInterval < time.Second {
		tickInterval = time.Second
	}

	m.warmRound(inactiveTTL, timeout)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.warmRound(inactiveTTL, timeout)
		}
	}
}

func (m *ProxyWarmManager) warmRound(inactiveTTL, timeout time.Duration) {
	m.mu.Lock()
	// Collect hosts ready for warming
	now := time.Now()
	for host, state := range m.hosts {
		// Remove inactive hosts
		if m.isInactive(state, now, inactiveTTL) {
			delete(m.hosts, host)
			log.Debugf("proxy_warmup: removing inactive host %s", host)
			continue
		}
	}
	// Build worklist
	type warmJob struct {
		host  string
		state *warmHostState
	}
	var worklist []warmJob
	for host, state := range m.hosts {
		if now.Before(state.nextWarmAt) {
			continue
		}
		if m.cfg.MaxHostsPerProxy > 0 && len(worklist) >= m.cfg.MaxHostsPerProxy {
			break
		}
		worklist = append(worklist, warmJob{host: host, state: state})
	}
	m.mu.Unlock()

	for _, job := range worklist {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		m.doWarm(job.host, job.state, timeout)
	}
}

func (m *ProxyWarmManager) doWarm(host string, state *warmHostState, timeout time.Duration) {
	transport := cachedProxyTransport(m.proxyURL, m.sdkCfg)
	if transport == nil {
		log.Debugf("proxy_warmup: no transport for %s", m.proxyURL)
		return
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	method := strings.ToUpper(strings.TrimSpace(state.targetMethod))
	if method == "" {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(ctx, method, state.targetURL, nil)
	if err != nil {
		log.Debugf("proxy_warmup: failed to create request for %s: %v", host, err)
		m.markFailure(state)
		return
	}
	req.Header.Set("User-Agent", "CliRelay-Warmup/1.0")

	started := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(started)

	var drainErr error
	if resp != nil && resp.Body != nil {
		_, drainErr = io.Copy(io.Discard, resp.Body)
		if closeErr := resp.Body.Close(); drainErr == nil {
			drainErr = closeErr
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		log.Debugf("proxy_warmup: %s failed after %v: %v", host, elapsed, err)
		m.markFailureLocked(state)
		return
	}
	if drainErr != nil {
		log.Debugf("proxy_warmup: %s response drain failed after %v: %v", host, elapsed, drainErr)
		m.markFailureLocked(state)
		return
	}

	// Only log success at debug; avoid noisy success logs
	log.Debugf("proxy_warmup: %s %s -> %d in %v", method, host, resp.StatusCode, elapsed)
	m.markSuccessLocked(state)
}

func (m *ProxyWarmManager) markFailure(state *warmHostState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markFailureLocked(state)
}

func (m *ProxyWarmManager) markFailureLocked(state *warmHostState) {
	state.consecutiveFails++
	// Exponential backoff: 1min -> 2min -> 5min -> 10min -> capped at 10min
	backoff := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	idx := state.consecutiveFails - 1
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	state.nextWarmAt = time.Now().Add(backoff[idx])
}

func (m *ProxyWarmManager) markSuccessLocked(state *warmHostState) {
	state.consecutiveFails = 0
	jitter := randomJitter(m.cfg.IntervalJitterSeconds)
	state.nextWarmAt = time.Now().Add(time.Duration(m.cfg.IntervalSeconds) * time.Second).Add(jitter)
}

func (m *ProxyWarmManager) isInactive(state *warmHostState, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return now.Sub(state.lastUsed) > ttl
}

func (m *ProxyWarmManager) isHostAllowed(host string) bool {
	if len(m.cfg.AllowedHostSuffixes) == 0 {
		return true
	}
	host = normalizeWarmHost(host)
	for _, suffix := range m.cfg.AllowedHostSuffixes {
		suffix = strings.ToLower(strings.TrimSpace(suffix))
		if suffix == "" {
			continue
		}
		suffix = strings.TrimPrefix(suffix, "*.")
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (m *ProxyWarmManager) seedConfiguredTargets() {
	if m == nil || len(m.cfg.Targets) == 0 {
		return
	}
	now := time.Now()
	for _, target := range m.cfg.Targets {
		host := normalizeWarmHost(target.Host)
		if host == "" || target.URL == "" || !m.isHostAllowed(host) {
			continue
		}
		method := strings.ToUpper(strings.TrimSpace(target.Method))
		if method == "" {
			method = http.MethodHead
		}
		if _, exists := m.hosts[host]; exists {
			continue
		}
		m.hosts[host] = &warmHostState{
			host:         host,
			targetURL:    strings.TrimSpace(target.URL),
			targetMethod: method,
			lastUsed:     now,
			nextWarmAt:   now,
		}
	}
}

// warmingRoundTripper wraps an http.RoundTripper and records host usage for connection warming.
type warmingRoundTripper struct {
	base    http.RoundTripper
	manager *ProxyWarmManager
}

func (w *warmingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if w.manager != nil && req.URL != nil {
		w.manager.MarkUsed(req.URL.Host)
	}
	base := w.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func warmDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

func randomJitter(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(rand.Intn(seconds*1000)) * time.Millisecond
}

func normalizeWarmHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if strings.Contains(host, "://") {
		if req, err := http.NewRequest(http.MethodGet, host, nil); err == nil && req.URL != nil {
			host = req.URL.Host
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(host, "[]")
}
