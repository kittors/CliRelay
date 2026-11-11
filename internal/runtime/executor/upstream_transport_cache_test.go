package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// The executors take their upstream transports from this cache, so this is
// where a transport without the HTTP/2 liveness check would reach production.
func TestUpstreamTransportCacheReturnsTransportsThatCheckHTTP2Liveness(t *testing.T) {
	cache := newUpstreamTransportCache()
	cases := map[string]*http.Transport{
		"direct":       cache.get("", nil, true),
		"http proxy":   cache.get("http://127.0.0.1:8080", nil, false),
		"socks5 proxy": cache.get("socks5://127.0.0.1:1080", nil, false),
	}
	for name, transport := range cases {
		t.Run(name, func(t *testing.T) {
			if transport == nil {
				t.Fatal("cache returned no transport")
			}
			h2 := transport.HTTP2
			if h2 == nil {
				t.Fatal("HTTP2 is nil, want the liveness check from util")
			}
			if h2.SendPingTimeout != util.DefaultHTTP2ReadIdleTimeout || h2.PingTimeout != util.DefaultHTTP2PingTimeout {
				t.Fatalf("HTTP2 = %+v, want SendPingTimeout %s and PingTimeout %s",
					*h2, util.DefaultHTTP2ReadIdleTimeout, util.DefaultHTTP2PingTimeout)
			}
		})
	}
}
