package serviceapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisterExecutorForSynthesizedMiniMaxAuthUsesImageEndpoint(t *testing.T) {
	paths := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/image_generation" {
			http.Error(w, `{"error":{"message":"unexpected chat endpoint"}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"image_base64":["AAAA"]},"base_resp":{"status_code":0}}`)
	}))
	t.Cleanup(upstream.Close)
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "MiniMax", BaseURL: upstream.URL + "/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "test-key"}},
		Models:        []config.OpenAICompatibilityModel{{Name: "image-01"}},
	}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 1 {
		t.Fatalf("Synthesize: auth count = %d, error = %v", len(auths), err)
	}
	auth := auths[0]
	if auth.Provider != "minimax" || auth.Attributes["compat_name"] != "MiniMax" {
		t.Fatalf("synthesized auth did not preserve provider/compat metadata: %+v", auth)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	RegisterExecutorForAuth(manager, cfg, auth, false, nil)
	bound, ok := manager.Executor("minimax")
	if !ok || bound == nil {
		t.Fatal("MiniMax executor was not registered")
	}
	// Config credentials retain compat metadata for chat; images must still use
	// the dedicated executor, not the earlier generic compatibility branch.
	if _, ok := bound.(*executor.MiniMaxExecutor); !ok {
		t.Errorf("executor = %T, want *executor.MiniMaxExecutor", bound)
	}
	resp, err := bound.Execute(context.Background(), auth, coreexecutor.Request{
		Model: "image-01", Payload: []byte(`{"model":"image-01","prompt":"a cat"}`),
		Format: sdktranslator.FromString("openai"),
	}, coreexecutor.Options{Alt: "images/generations", SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Errorf("image Execute returned error: %v", err)
	}
	select {
	case path := <-paths:
		if path != "/v1/image_generation" {
			t.Errorf("upstream path = %q, want /v1/image_generation", path)
		}
	default:
		t.Error("image Execute did not reach the mock upstream")
	}
	if err == nil && gjson.GetBytes(resp.Payload, "data.0.b64_json").String() != "AAAA" {
		t.Errorf("image response = %s, want translated image", resp.Payload)
	}
}
