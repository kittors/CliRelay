package tlsfingerprint

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// Options configures a fingerprinting RoundTripper.
type Options struct {
	// Profile names the ClientHello template; empty selects DefaultProfile.
	Profile string
	// ProxyURL routes connections through an HTTP, HTTPS or SOCKS5 proxy.
	// Empty dials directly.
	ProxyURL string
	// PreferIPv4 restricts dialing to IPv4, matching the rest of the proxy's
	// egress behaviour.
	PreferIPv4 bool
	// DialTimeout bounds TCP establishment and the TLS handshake.
	DialTimeout time.Duration
	// InsecureSkipVerify disables certificate verification. It exists only for
	// operators terminating upstream TLS on an inspection proxy.
	InsecureSkipVerify bool
	// CACertPath adds a PEM bundle as the trust root, for the same inspection
	// proxy setups. Empty uses the system pool.
	CACertPath string
}

const defaultDialTimeout = 30 * time.Second

// Health-check bounds for the pooled HTTP/2 connections.
//
// h2Conns keeps one connection per host for as long as CanTakeNewRequest
// reports it usable, but that method only inspects local state: whether the
// connection was closed, whether a GOAWAY arrived, and whether the stream
// budget is exhausted. It cannot observe a peer that stopped answering without
// sending FIN or RST, which is what happens when a load balancer or NAT drops
// an idle flow. A request written onto such a connection is retransmitted by
// the kernel until tcp_retries2 is exhausted, so the caller waits roughly
// fifteen minutes for an error instead of seconds.
//
// Setting ReadIdleTimeout makes the connection's read loop send a PING once no
// frame has arrived for that long; PingTimeout bounds the wait for the PONG,
// after which closeForLostPing tears the connection down and the next
// CanTakeNewRequest returns false, forcing a redial. Detection is therefore
// bounded by their sum rather than by the kernel's retransmission budget.
// Both are variables rather than constants so tests can shorten them; nothing
// outside this package writes to them.
var (
	h2ReadIdleTimeout = 30 * time.Second
	h2PingTimeout     = 15 * time.Second
)

// RoundTripper performs HTTPS requests over connections whose ClientHello
// matches a configured client profile.
//
// It manages HTTP/2 connections itself rather than delegating to
// http.Transport: Go only routes a connection to its HTTP/2 stack when the
// connection is a *crypto/tls.Conn, and a utls connection never is. Handing a
// utls connection to http.Transport would therefore speak HTTP/1.1 on a
// connection where the server already agreed to HTTP/2 via ALPN.
type RoundTripper struct {
	opts    Options
	helloID utls.ClientHelloID
	// rootCAs is resolved once at construction so a missing or malformed bundle
	// is reported at setup instead of on every handshake.
	rootCAs *x509.CertPool

	mu sync.Mutex
	// h2Conns caches one HTTP/2 connection per host. HTTP/2 multiplexes, so a
	// single connection carries concurrent requests.
	h2Conns map[string]*http2.ClientConn
	// dialing serializes connection setup per host so a burst of requests to a
	// cold host opens one connection instead of one per request.
	dialing map[string]*sync.Cond
}

// New builds a RoundTripper for the given options. It returns an error for an
// unknown profile or an unparseable proxy URL so misconfiguration surfaces at
// setup instead of as a per-request failure.
func New(opts Options) (*RoundTripper, error) {
	helloID, ok := ResolveProfile(opts.Profile)
	if !ok {
		return nil, fmt.Errorf("tlsfingerprint: unknown profile %q, want one of %s",
			opts.Profile, strings.Join(AvailableProfiles(), ", "))
	}
	if proxyURL := strings.TrimSpace(opts.ProxyURL); proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("tlsfingerprint: parse proxy URL: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("tlsfingerprint: unsupported proxy scheme %q", parsed.Scheme)
		}
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = defaultDialTimeout
	}

	var rootCAs *x509.CertPool
	if caPath := strings.TrimSpace(opts.CACertPath); caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("tlsfingerprint: read CA cert %s: %w", caPath, err)
		}
		rootCAs = x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tlsfingerprint: parse CA cert %s", caPath)
		}
	}

	return &RoundTripper{
		opts:    opts,
		helloID: helloID,
		rootCAs: rootCAs,
		h2Conns: make(map[string]*http2.ClientConn),
		dialing: make(map[string]*sync.Cond),
	}, nil
}

// RoundTrip implements http.RoundTripper.
func (t *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return nil, errors.New("tlsfingerprint: request has no URL")
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		// Plaintext requests carry no ClientHello, so there is nothing to
		// fingerprint and no reason for this transport to handle them.
		return nil, fmt.Errorf("tlsfingerprint: scheme %q is not supported, want https", req.URL.Scheme)
	}

	host := req.URL.Hostname()
	addr := canonicalAddr(req.URL)

	h2Conn, err := t.h2ConnFor(req.Context(), host, addr)
	if err != nil {
		return nil, err
	}
	if h2Conn != nil {
		resp, errRT := h2Conn.RoundTrip(req)
		if errRT != nil {
			t.dropH2Conn(host, h2Conn)
			return nil, errRT
		}
		return resp, nil
	}

	// The server declined HTTP/2 for this profile's ALPN list. Fall back to a
	// single-use HTTP/1.1 exchange; without Go's connection pool behind it there
	// is no keep-alive here, which is acceptable because every provider this
	// proxy targets negotiates HTTP/2.
	return t.roundTripHTTP1(req, host, addr)
}

// h2ConnFor returns a usable HTTP/2 connection for host, or nil when the
// handshake settled on HTTP/1.1.
func (t *RoundTripper) h2ConnFor(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()
	for {
		if conn, ok := t.h2Conns[host]; ok {
			if conn.CanTakeNewRequest() {
				t.mu.Unlock()
				return conn, nil
			}
			delete(t.h2Conns, host)
		}
		cond, inFlight := t.dialing[host]
		if !inFlight {
			break
		}
		// Another goroutine is dialing this host; wait for it and re-check.
		cond.Wait()
	}

	cond := sync.NewCond(&t.mu)
	t.dialing[host] = cond
	t.mu.Unlock()

	conn, err := t.dialH2(ctx, host, addr)

	t.mu.Lock()
	delete(t.dialing, host)
	cond.Broadcast()
	if err == nil && conn != nil {
		t.h2Conns[host] = conn
	}
	t.mu.Unlock()

	return conn, err
}

// dialH2 establishes a fingerprinted TLS connection and, when ALPN selected
// HTTP/2, wraps it in an HTTP/2 client connection. A nil connection with a nil
// error means the peer chose HTTP/1.1.
func (t *RoundTripper) dialH2(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	tlsConn, err := t.dialTLS(ctx, host, addr)
	if err != nil {
		return nil, err
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
		// Closed here because the HTTP/1.1 path re-dials: reusing this
		// connection would require threading it back out through the cache,
		// which is not worth it for a path the target providers never take.
		_ = tlsConn.Close()
		return nil, nil
	}
	h2Transport := &http2.Transport{
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
	}
	h2Conn, err := h2Transport.NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return h2Conn, nil
}

// dialTLS opens a TCP connection through the configured egress path and
// performs the fingerprinted TLS handshake on top of it.
func (t *RoundTripper) dialTLS(ctx context.Context, host, addr string) (*utls.UConn, error) {
	rawConn, err := t.dialTCP(ctx, addr)
	if err != nil {
		return nil, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(t.opts.DialTimeout))
	}

	tlsConn := utls.UClient(rawConn, &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: t.opts.InsecureSkipVerify,
		RootCAs:            t.rootCAs,
	}, t.helloID)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	// Clear the handshake deadline: streaming responses must not inherit it.
	_ = rawConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// dialTCP establishes the underlying TCP connection, tunnelling through a proxy
// when one is configured.
func (t *RoundTripper) dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	proxyURL := strings.TrimSpace(t.opts.ProxyURL)
	if proxyURL == "" {
		return t.baseDialer().DialContext(ctx, t.network(), addr)
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("tlsfingerprint: parse proxy URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks5", "socks5h":
		return t.dialViaSOCKS5(ctx, parsed, addr)
	case "http", "https":
		return t.dialViaHTTPProxy(ctx, parsed, addr)
	default:
		return nil, fmt.Errorf("tlsfingerprint: unsupported proxy scheme %q", parsed.Scheme)
	}
}

func (t *RoundTripper) baseDialer() *net.Dialer {
	return &net.Dialer{Timeout: t.opts.DialTimeout, KeepAlive: 30 * time.Second}
}

func (t *RoundTripper) network() string {
	if t.opts.PreferIPv4 {
		return "tcp4"
	}
	return "tcp"
}

func (t *RoundTripper) dialViaSOCKS5(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5(t.network(), proxyURL.Host, auth, t.baseDialer())
	if err != nil {
		return nil, err
	}
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, t.network(), addr)
	}
	return dialer.Dial(t.network(), addr)
}

// dialViaHTTPProxy opens a CONNECT tunnel. http.Transport would normally handle
// this, but it only exposes the tunnelled connection to its own TLS stack, so
// the tunnel has to be established here for the utls handshake to run on top.
func (t *RoundTripper) dialViaHTTPProxy(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		if strings.EqualFold(proxyURL.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
	}

	conn, err := t.baseDialer().DialContext(ctx, t.network(), proxyAddr)
	if err != nil {
		return nil, err
	}

	// An HTTPS proxy speaks TLS on the hop to the proxy itself. That handshake
	// uses the standard library on purpose: the fingerprint that matters is the
	// one the origin server sees, inside the tunnel.
	if strings.EqualFold(proxyURL.Scheme, "https") {
		proxyTLS := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname()})
		if err = proxyTLS.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = proxyTLS
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		connectReq.Header.Set("Proxy-Authorization", basicProxyAuth(proxyURL.User.Username(), password))
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(t.opts.DialTimeout))
	}
	if err = connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Buffered reads must stop at the end of the CONNECT response: anything the
	// reader buffers past it belongs to the TLS handshake that follows.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("tlsfingerprint: proxy CONNECT to %s failed: %s", addr, resp.Status)
	}
	if reader.Buffered() > 0 {
		_ = conn.Close()
		return nil, errors.New("tlsfingerprint: proxy sent data before the tunnel was established")
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// roundTripHTTP1 serves one request over a dedicated fingerprinted connection.
func (t *RoundTripper) roundTripHTTP1(req *http.Request, host, addr string) (*http.Response, error) {
	tlsConn, err := t.dialTLS(req.Context(), host, addr)
	if err != nil {
		return nil, err
	}
	if err = req.Write(tlsConn); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	// The caller closes the body; closing the connection with it prevents the
	// socket leaking, since this path keeps no pool.
	resp.Body = &connClosingBody{ReadCloser: resp.Body, conn: tlsConn}
	return resp, nil
}

// connClosingBody ties a response body to the connection that produced it, so
// closing the body releases the socket on this pool-less path.
type connClosingBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	if errConn := b.conn.Close(); err == nil {
		err = errConn
	}
	return err
}

func (t *RoundTripper) dropH2Conn(host string, conn *http2.ClientConn) {
	t.mu.Lock()
	if cached, ok := t.h2Conns[host]; ok && cached == conn {
		delete(t.h2Conns, host)
	}
	t.mu.Unlock()
}

// CloseIdleConnections implements the optional http.Transport behaviour used by
// http.Client.CloseIdleConnections.
func (t *RoundTripper) CloseIdleConnections() {
	t.mu.Lock()
	conns := make([]*http2.ClientConn, 0, len(t.h2Conns))
	for host, conn := range t.h2Conns {
		conns = append(conns, conn)
		delete(t.h2Conns, host)
	}
	t.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// canonicalAddr returns host:port, defaulting to the HTTPS port.
func canonicalAddr(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(host, port)
}

func basicProxyAuth(username, password string) string {
	req := &http.Request{Header: make(http.Header)}
	req.SetBasicAuth(username, password)
	return req.Header.Get("Authorization")
}
