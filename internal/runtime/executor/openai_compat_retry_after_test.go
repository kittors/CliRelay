package executor_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestOpenAICompatRetryAfterControlsManagerCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct {
			name, header string
			want         time.Duration
			status       int
		}{
			{"short delay", "7", 7 * time.Second, http.StatusTooManyRequests},
			{"long delay", "120", 120 * time.Second, http.StatusTooManyRequests},
			{"HTTP date", time.Now().Add(120 * time.Second).UTC().Format(http.TimeFormat), 120 * time.Second, http.StatusTooManyRequests},
			{"missing", "", time.Minute, http.StatusTooManyRequests},
			{"invalid", "not-a-delay", time.Minute, http.StatusTooManyRequests},
			{"negative", "-1", time.Minute, http.StatusTooManyRequests},
			{"zero", "0", time.Minute, http.StatusTooManyRequests},
			{"overflow", "9223372037", time.Minute, http.StatusTooManyRequests},
			{"max integer", "9223372036854775807", time.Minute, http.StatusTooManyRequests},
			{"positive wrap", "18446744074", time.Minute, http.StatusTooManyRequests},
			// A 503 that names a time is believed too (bounded by the scheduler);
			// without one it keeps the minute it always had.
			{"503 follows Retry-After", "120", 120 * time.Second, http.StatusServiceUnavailable},
			{"503 without Retry-After", "", time.Minute, http.StatusServiceUnavailable},
			{"503 with overflow", "9223372037", time.Minute, http.StatusServiceUnavailable},
			{"500 ignores Retry-After", "120", time.Minute, http.StatusInternalServerError},
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, tt.name), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if tt.header != "" {
						w.Header().Set("Retry-After", tt.header)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
				}))
				defer server.Close()
				const provider = "retry-after-test"
				const model = "retry-after-model"
				id := t.Name()
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0)
				manager.RegisterExecutor(executor.NewOpenAICompatExecutor(provider, &config.Config{}))
				auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test-key"}}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: provider}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req := coreexecutor.Request{Model: model, Payload: []byte(`{"model":"retry-after-model","messages":[{"role":"user","content":"hello"}]}`)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
				before := time.Now()
				var err error
				if stream {
					_, err = manager.ExecuteStream(ctx, []string{provider}, req, opts)
				} else {
					_, err = manager.Execute(ctx, []string{provider}, req, opts)
				}
				if err == nil {
					t.Fatal("expected upstream failure")
				}
				details, ok := err.(interface {
					StatusCode() int
					Headers() http.Header
					RetryAfter() *time.Duration
				})
				if !ok {
					t.Fatalf("upstream error metadata lost: %T %v", err, err)
				}
				if details.StatusCode() != tt.status || details.Headers().Get("Retry-After") != tt.header || err.Error() != `{"error":{"message":"rate limit exceeded"}}` {
					t.Fatalf("status/header/body changed: %v", err)
				}
				// The executor publishes nothing; the scheduler reads the response
				// itself, so the error handed back to callers is exactly as received.
				if details.RetryAfter() != nil {
					t.Error("executor published a retry wait of its own")
				}
				if calls.Load() != 1 {
					t.Fatalf("upstream calls=%d, want 1", calls.Load())
				}
				updated, ok := manager.GetByID(id)
				if !ok {
					t.Fatal("registered auth missing")
				}
				state := updated.ModelStates[model]
				if state == nil {
					t.Fatalf("missing model state after error: %v", err)
				}
				delay := state.NextRetryAfter.Sub(before)
				if delay < tt.want-2*time.Second || delay > tt.want+2*time.Second {
					t.Errorf("cooldown=%s, want approximately %s", delay, tt.want)
				}
				if state.Quota.Exceeded {
					t.Error("transient rate limit marked as exhausted quota")
				}
			})
		}
	}
}
