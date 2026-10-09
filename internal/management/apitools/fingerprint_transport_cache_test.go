package apitools

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestCodexProbesShareOneFingerprintConnection is the regression test for
// leaked upstream connections: the quota probe resolved a fresh fingerprint
// transport on every call, so each probe dialed its own HTTP/2 connection and
// left it behind. The transport's PING health check then kept every abandoned
// connection alive, one more per Codex account every fifteen minutes.
func TestCodexProbesShareOneFingerprintConnection(t *testing.T) {
	resetFingerprintTransports := func() {
		managementTransportMu.Lock()
		managementFingerprintTransports = map[managementTransportKey]http.RoundTripper{}
		managementTransportMu.Unlock()
	}
	resetFingerprintTransports()
	// The cached transport still holds a connection to this test's server.
	t.Cleanup(resetFingerprintTransports)

	var open atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"plan_type":"plus"}`)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	server.StartTLS()
	defer server.Close()

	cfg := &config.Config{}
	// The test server is self-signed; production verifies normally.
	cfg.InsecureSkipVerify = true
	svc := NewForTenant("test-tenant", cfg, nil, Dependencies{})
	auth := &coreauth.Auth{Provider: "codex"}
	target, err := url.Parse(server.URL + "/backend-api/wham/usage")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	const probes = 5
	for i := 0; i < probes; i++ {
		// Mirrors aiaccountstatus.doAuthRequestStatus: a fresh client per probe,
		// with the transport resolved for the target URL.
		client := &http.Client{Timeout: 5 * time.Second, Transport: svc.APICallTransportForURL(auth, target)}
		resp, errGet := client.Get(target.String())
		if errGet != nil {
			t.Fatalf("probe %d: %v", i, errGet)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("probe %d negotiated %s, want HTTP/2", i, resp.Proto)
		}
	}

	if got := open.Load(); got != 1 {
		t.Fatalf("%d probes left %d upstream connections open, want 1 shared connection", probes, got)
	}
}
