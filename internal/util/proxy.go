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
	DefaultHTTPResponseHeaderTimout = 30 * time.Second
	DefaultHTTPIdleConnTimeout      = 90 * time.Second
)

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
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if preferIPv4 {
				network = "tcp4"
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
	return httpClient
}
