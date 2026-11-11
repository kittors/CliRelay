// Package util provides utility functions for the CLI Proxy API server.
// It includes helper functions for proxy configuration, HTTP client setup,
// log level management, and other common operations used across the application.
package util

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

const (
	DefaultHTTPClientTimeout        = 30 * time.Second
	DefaultHTTPDialTimeout          = 10 * time.Second
	DefaultHTTPTLSHandshakeTimeout  = 10 * time.Second
	DefaultHTTPResponseHeaderTimout = 600 * time.Second
	DefaultHTTPIdleConnTimeout      = 90 * time.Second
)

// HTTP/2 liveness check for upstream connections.
//
// net/http keeps sending requests on an HTTP/2 connection for as long as the
// connection looks usable locally. It cannot see a peer that went silent
// without a FIN or RST, which is what a network interruption leaves behind:
// behind a proxy the kernel still receives ACKs, so the connection never
// errors, yet nothing sent on it is answered. ResponseHeaderTimeout does not
// cover this. It starts only after the request body has been written, a body
// stalls on flow control once no WINDOW_UPDATE arrives, and a request that
// does time out leaves the connection in the pool for the next one.
//
// With SendPingTimeout set, a connection that has received no frame for
// DefaultHTTP2ReadIdleTimeout sends a PING and is closed when the reply takes
// longer than DefaultHTTP2PingTimeout. Requests on it then fail right away
// instead of waiting for the client to give up, and later requests open a
// fresh connection, so a dead connection is in use for at most the sum of the
// two. The values match the TLS fingerprint transports, which check their own
// HTTP/2 connections the same way.
//
// These PINGs also keep a connection alive on the wire, so a transport that
// sends them must keep IdleConnTimeout set; without it a connection nobody
// uses any more would never close.
const (
	DefaultHTTP2ReadIdleTimeout = 30 * time.Second
	DefaultHTTP2PingTimeout     = 15 * time.Second
)

// newHTTP2LivenessConfig returns a value of its own for each transport, so
// adjusting one transport's settings cannot change another's.
func newHTTP2LivenessConfig() *http.HTTP2Config {
	return &http.HTTP2Config{
		SendPingTimeout: DefaultHTTP2ReadIdleTimeout,
		PingTimeout:     DefaultHTTP2PingTimeout,
	}
}

func NewHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

func NewDefaultTransport(preferIPv4 bool) *http.Transport {
	dialer := &net.Dialer{Timeout: DefaultHTTPDialTimeout, KeepAlive: 30 * time.Second}
	dialContext := dialer.DialContext
	if preferIPv4 {
		dialer.LocalAddr = &net.TCPAddr{IP: net.IPv4zero}
		dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		}
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       DefaultHTTPIdleConnTimeout,
		TLSHandshakeTimeout:   DefaultHTTPTLSHandshakeTimeout,
		ResponseHeaderTimeout: DefaultHTTPResponseHeaderTimout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConnsPerHost:   20,
		HTTP2:                 newHTTP2LivenessConfig(),
	}
}

func BuildProxyTransport(proxyURL string, preferIPv4 bool) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("parse proxy URL failed: %v", errParse)
		return nil
	}

	transport := NewDefaultTransport(preferIPv4)
	switch parsedURL.Scheme {
	case "socks5":
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		proxyNetwork := "tcp"
		if preferIPv4 {
			proxyNetwork = "tcp4"
		}
		forwardDialer := &net.Dialer{Timeout: DefaultHTTPDialTimeout, KeepAlive: 30 * time.Second}
		dialer, errSOCKS5 := proxy.SOCKS5(proxyNetwork, parsedURL.Host, proxyAuth, forwardDialer)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, addr)
			}
			return dialer.Dial(network, addr)
		}
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsedURL)
	default:
		log.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	return transport
}

// SetProxy configures the provided HTTP client with proxy settings from the configuration.
// It supports SOCKS5, HTTP, and HTTPS proxies. The function modifies the client's transport
// to route requests through the configured proxy server.
func SetProxy(cfg *config.SDKConfig, httpClient *http.Client) *http.Client {
	if httpClient == nil {
		httpClient = NewHTTPClient(DefaultHTTPClientTimeout)
	}
	if cfg == nil {
		if httpClient.Transport == nil {
			httpClient.Transport = NewDefaultTransport(false)
		}
		return httpClient
	}
	if httpClient.Transport == nil {
		httpClient.Transport = NewDefaultTransport(cfg.PreferIPv4)
	}
	if transport := BuildProxyTransport(cfg.ProxyURL, cfg.PreferIPv4); transport != nil {
		httpClient.Transport = transport
	}
	if t, ok := httpClient.Transport.(*http.Transport); ok {
		ApplyTLSConfig(t, cfg)
	}
	return httpClient
}
