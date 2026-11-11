package executor

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

func newProxyAwareWebsocketDialer(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := ""
	if cfg != nil {
		proxyID := ""
		fallbackURL := ""
		if auth != nil {
			proxyID = auth.ProxyID
			fallbackURL = auth.ProxyURL
		}
		proxyURL = cfg.ResolveProxyURL(proxyID, fallbackURL)
	} else if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	recordRequestLogEgressRoute(ctx, cfg, auth, proxyURL)
	if proxyURL == "" {
		return dialer
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: parse proxy URL failed: %v", errParse)
		return dialer
	}

	switch parsedURL.Scheme {
	case "socks5":
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		// Failures of the proxy hop itself are marked where they happen (see
		// package egressbreaker), as util.BuildProxyTransport marks them for
		// HTTP requests. The marks do not change the error text.
		socksDialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, egressbreaker.HopDialer{Base: proxy.Direct.DialContext})
		if errSOCKS5 != nil {
			log.Errorf("codex websockets executor: create SOCKS5 dialer failed: %v", errSOCKS5)
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			conn, err := socksDialer.Dial(network, addr)
			return conn, egressbreaker.MarkSOCKS5(err)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(parsedURL)
		// With a proxy set this dial only ever connects to the proxy. The
		// CONNECT exchange is gorilla's own and reports a 407 as plain text,
		// so a rejected authentication is not recognised on this path.
		dialer.NetDialContext = egressbreaker.MarkDial(dialer.NetDialContext)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", parsedURL.Scheme)
	}

	return dialer
}
