package dnswatch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DialContextFunc matches net.Dialer.DialContext. Tests use it to route a
// node's public address to a local listener.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// probeResult is the outcome of one readiness probe.
type probeResult struct {
	ok      bool
	status  int
	latency time.Duration
	// reason explains a failure; it is empty when ok.
	reason string
}

// prober sends the readiness probe, and the egress probe when configured, to
// each node.
type prober struct {
	client  *http.Client
	host    string
	path    string
	timeout time.Duration
	expect  map[int]bool
	// egressPath is empty when the egress probe is off.
	egressPath string
}

// newProber builds the probe client. roots nil means the system trust store.
func newProber(cfg ProbeConfig, roots *x509.CertPool, dial DialContextFunc) *prober {
	if dial == nil {
		dial = (&net.Dialer{Timeout: cfg.Timeout}).DialContext
	}
	transport := &http.Transport{
		// Probes must reach the node itself, never an egress proxy picked up
		// from the environment.
		Proxy:       nil,
		DialContext: dial,
		TLSClientConfig: &tls.Config{
			// The URL carries the node IP, so the certificate is checked
			// against the public hostname instead. Verification stays on: a
			// node serving a wrong or expired certificate is down for every
			// real client and must count as down here too.
			ServerName: cfg.Host,
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
		// A fresh TCP and TLS handshake per probe exercises the path a new
		// client takes; a reused connection could hide a dead listener.
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	expect := make(map[int]bool, len(cfg.ExpectStatus))
	for _, code := range cfg.ExpectStatus {
		expect[code] = true
	}
	return &prober{
		client: &http.Client{
			Transport: transport,
			// A redirect means the node is not answering the readiness path
			// itself, so it is judged on the redirect status.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		host:       cfg.Host,
		path:       cfg.Path,
		timeout:    cfg.Timeout,
		expect:     expect,
		egressPath: cfg.EgressPath,
	}
}

// probe sends GET https://<ip><path> with the configured Host and SNI. Each
// call has its own timeout, so one hung node cannot eat another's budget.
func (p *prober) probe(ctx context.Context, ip string) probeResult {
	return p.get(ctx, ip, p.path, func(status int) bool { return p.expect[status] })
}

// probeEgress requests the egress path the same way. Only a 2xx passes: a 404
// from a node that predates the endpoint fails too, so no node is assumed to
// have working egress without saying so.
func (p *prober) probeEgress(ctx context.Context, ip string) probeResult {
	return p.get(ctx, ip, p.egressPath, func(status int) bool { return status/100 == 2 })
}

// get sends GET https://<ip><path> and judges the status with accept.
func (p *prober) get(ctx context.Context, ip, path string, accept func(int) bool) probeResult {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+ip+path, nil)
	if err != nil {
		return probeResult{reason: err.Error()}
	}
	req.Host = p.host
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return probeResult{latency: time.Since(start), reason: describeProbeError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	result := probeResult{status: resp.StatusCode, latency: time.Since(start)}
	if !accept(resp.StatusCode) {
		result.reason = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return result
	}
	result.ok = true
	return result
}

// probeAll probes every node's readiness concurrently and returns results in
// node order.
func (p *prober) probeAll(ctx context.Context, nodes []Node) []probeResult {
	ready, _ := p.probeRound(ctx, nodes)
	return ready
}

// probeRound sends every probe of one round at once: readiness to every node,
// plus the egress probe when it is on (egress is nil otherwise). Results are
// in node order.
func (p *prober) probeRound(ctx context.Context, nodes []Node) (ready, egress []probeResult) {
	ready = make([]probeResult, len(nodes))
	if p.egressPath != "" {
		egress = make([]probeResult, len(nodes))
	}
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Go(func() { ready[i] = p.probe(ctx, node.IP) })
		if egress != nil {
			wg.Go(func() { egress[i] = p.probeEgress(ctx, node.IP) })
		}
	}
	wg.Wait()
	return ready, egress
}

// describeProbeError turns a client error into a short reason for logs,
// without the URL wrapper that would repeat the node address.
func describeProbeError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	return err.Error()
}
