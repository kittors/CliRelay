package executor

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/proxytest"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// directEgress stands in for the host's own network. The executors hand a
// request to the context's round tripper only when no proxy was resolved for
// it, so every request it sees would have left directly. It fails them instead
// of sending them, which keeps a test that catches such a request away from the
// real Kimi gateway.
type directEgress struct{ requests atomic.Int32 }

func (d *directEgress) RoundTrip(*http.Request) (*http.Response, error) {
	d.requests.Add(1)
	return nil, errors.New("directEgress: the request left without a proxy")
}

func kimiEgressTestAuth(proxyID string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "kimi-account",
		Provider: "kimi",
		ProxyID:  proxyID,
		// The Claude-format path writes the gateway's base_url into the
		// attributes, as it does on the copy the scheduler hands it.
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "kimi-test-token"},
	}
}

// Kimi serves Claude-format requests, and every token count, from the
// Anthropic-compatible endpoint of its gateway through a ClaudeExecutor. They
// have to leave through the same proxy as its OpenAI-format requests: the
// proxy-pool entry the credential names, else the global proxy-url. That
// ClaudeExecutor used to be a zero value without a configuration, so neither was
// resolved and the requests went out directly from the host.
func TestKimiRequestsLeaveThroughTheConfiguredProxyInEveryFormat(t *testing.T) {
	const model = "kimi-k2"
	claudeBody := []byte(`{"model":"kimi-k2","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	openAIBody := []byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"hello"}]}`)
	claudeFormat := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	openAIFormat := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	requests := []struct {
		name string
		send func(ctx context.Context, exec *KimiExecutor, auth *cliproxyauth.Auth) error
	}{
		{"claude/Execute", func(ctx context.Context, exec *KimiExecutor, auth *cliproxyauth.Auth) error {
			_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: claudeBody}, claudeFormat)
			return err
		}},
		{"claude/ExecuteStream", func(ctx context.Context, exec *KimiExecutor, auth *cliproxyauth.Auth) error {
			result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: claudeBody}, claudeFormat)
			if result != nil {
				for range result.Chunks {
				}
			}
			return err
		}},
		{"claude/CountTokens", func(ctx context.Context, exec *KimiExecutor, auth *cliproxyauth.Auth) error {
			_, err := exec.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: claudeBody}, claudeFormat)
			return err
		}},
		// The OpenAI-format path always used the executor's configuration. It is
		// here so that the test states what both formats share.
		{"openai/Execute", func(ctx context.Context, exec *KimiExecutor, auth *cliproxyauth.Auth) error {
			_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: openAIBody}, openAIFormat)
			return err
		}},
	}
	sources := []struct {
		name      string
		configure func(proxyURL string) (*config.Config, *cliproxyauth.Auth)
	}{
		{"proxy-pool entry", func(proxyURL string) (*config.Config, *cliproxyauth.Auth) {
			cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{
				{ID: "pool", Name: "Pool", URL: proxyURL, Enabled: true},
			}}
			return cfg, kimiEgressTestAuth("pool")
		}},
		{"global proxy-url", func(proxyURL string) (*config.Config, *cliproxyauth.Auth) {
			cfg := &config.Config{}
			cfg.ProxyURL = proxyURL
			return cfg, kimiEgressTestAuth("")
		}},
	}

	for _, source := range sources {
		for _, request := range requests {
			t.Run(source.name+"/"+request.name, func(t *testing.T) {
				proxy := proxytest.NewSOCKS5(t)
				// The CONNECT target is the real Kimi gateway, so the proxy
				// answers every CONNECT with "host unreachable" (0x04): reaching
				// the proxy is what is checked, and nothing leaves the machine.
				// The egress breaker does not hold that reply against the proxy.
				proxy.ReplyToConnect(0x04)
				cfg, auth := source.configure(proxy.URL("", ""))
				direct := &directEgress{}
				ctx := cliproxyexecutor.WithRoundTripper(context.Background(), direct)

				err := request.send(ctx, NewKimiExecutor(cfg), auth)
				if err == nil {
					t.Fatal("the request succeeded although the proxy refuses every CONNECT")
				}
				if got := direct.requests.Load(); got != 0 {
					t.Fatalf("%d request(s) left without the configured proxy (%v)", got, err)
				}
				if got := proxy.Connects(); got != 1 {
					t.Fatalf("the proxy saw %d CONNECT requests, want 1 (%v)", got, err)
				}
			})
		}
	}
}
