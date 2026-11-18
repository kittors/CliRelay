package api

import (
	"testing"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildHTTPServerListenAddr(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{name: "all interfaces", host: "", want: ":8318"},
		{name: "ipv4 loopback", host: "127.0.0.1", want: "127.0.0.1:8318"},
		{name: "ipv6 loopback is bracketed", host: "::1", want: "[::1]:8318"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := buildHTTPServer(&proxyconfig.Config{Host: tc.host, Port: 8318}, gin.New())
			if server.Addr != tc.want {
				t.Fatalf("Addr = %q, want %q", server.Addr, tc.want)
			}
		})
	}
}
