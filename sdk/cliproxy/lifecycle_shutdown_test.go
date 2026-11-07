package cliproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// The window bounds how long a terminating process may spend finishing requests
// that are already streaming. Five minutes is the floor for an LLM proxy; the
// previous fixed 30s cut long answers off mid-stream on every deploy.
func TestShutdownGracePeriodDefaultsToFiveMinutes(t *testing.T) {
	t.Setenv("CLIRELAY_SHUTDOWN_GRACE", "")
	if got := shutdownGracePeriod(); got != 5*time.Minute {
		t.Fatalf("grace period = %s, want 5m", got)
	}
}

func TestShutdownGracePeriodHonoursOverride(t *testing.T) {
	t.Setenv("CLIRELAY_SHUTDOWN_GRACE", "90s")
	if got := shutdownGracePeriod(); got != 90*time.Second {
		t.Fatalf("grace period = %s, want 90s", got)
	}
}

// A malformed or non-positive override must not silently disable the wait —
// falling back to zero would make every deploy sever in-flight requests.
func TestShutdownGracePeriodRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{"nonsense", "0", "-30s"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CLIRELAY_SHUTDOWN_GRACE", raw)
			if got := shutdownGracePeriod(); got != 5*time.Minute {
				t.Fatalf("grace period for %q = %s, want the 5m default", raw, got)
			}
		})
	}
}

// TestRunDrainsInFlightRequestsAfterOutlivingTheGracePeriod is the deploy
// cut-off of 2026-10-13 (75 requests) and 2026-10-19 (19 requests). Run created
// the grace window when it started, so in any process up longer than the window
// — every production slot by the time a deploy stops it — SIGTERM handed
// Shutdown a deadline that had already passed. The server logged "entering
// draining mode" and "shutdown timed out" milliseconds apart, Run returned, and
// the process exited with answers still streaming.
func TestRunDrainsInFlightRequestsAfterOutlivingTheGracePeriod(t *testing.T) {
	// Short enough for the process to outlive, long enough that the drain
	// below ends well inside a window that opens when shutdown does.
	const grace = time.Second
	t.Setenv("CLIRELAY_SHUTDOWN_GRACE", grace.String())

	var enterOnce, releaseOnce sync.Once
	entered := make(chan struct{})
	finish := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(finish) }) }
	t.Cleanup(release)

	port := freeLoopbackPort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	listening := make(chan struct{})
	service := &Service{
		cfg:            &config.Config{AuthDir: t.TempDir(), Host: "127.0.0.1", Port: port},
		configPath:     filepath.Join(t.TempDir(), "config.yaml"),
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		watcherFactory: stubWatcherFactory,
		coreManager:    storedAuthsManager(),
		serverOptions: []api.ServerOption{api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			// Stands in for a streamed answer: open until the test lets it end.
			engine.GET("/test/in-flight", func(c *gin.Context) {
				enterOnce.Do(func() { close(entered) })
				<-finish
				c.String(http.StatusOK, "finished")
			})
		})},
	}
	service.hooks.OnAfterStart = func(*Service) { close(listening) }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	began := time.Now()
	go func() { runErr <- service.Run(ctx) }()
	select {
	case <-listening:
	case err := <-runErr:
		t.Fatalf("Run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the API server did not start")
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	waitForCondition(t, "the API server to accept connections", func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode/100 == 2
	})

	time.Sleep(time.Until(began.Add(grace + 200*time.Millisecond)))

	type answer struct {
		status int
		body   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := client.Get(base + "/test/in-flight")
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		answered <- answer{status: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached its handler")
	}

	cancel() // what SIGTERM does to the process
	select {
	case err := <-runErr:
		t.Fatalf("Run returned (%v) with a request still in flight; the process would exit and cut it off", err)
	case <-time.After(grace / 4):
	}

	release()
	select {
	case got := <-answered:
		if got.err != nil || got.status != http.StatusOK || got.body != "finished" {
			t.Fatalf("in-flight request ended with status %d body %q err %v, want 200 \"finished\"", got.status, got.body, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request did not complete during shutdown")
	}
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the last in-flight request finished")
	}
}

// freeLoopbackPort names a loopback port that was free a moment ago.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback port %d: %v", port, err)
	}
	return port
}
