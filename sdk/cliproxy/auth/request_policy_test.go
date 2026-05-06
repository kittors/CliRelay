package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestPolicyTestExecutor struct {
	id string

	mu    sync.Mutex
	calls []string
}

func (e *requestPolicyTestExecutor) Identifier() string { return e.id }

func (e *requestPolicyTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, authID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *requestPolicyTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *requestPolicyTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestPolicyTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *requestPolicyTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *requestPolicyTestExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	copy(out, e.calls)
	return out
}

func TestManagerExecute_RequestPolicySkipChannelFallsBack(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	bigmodel := &requestPolicyTestExecutor{id: "bigmodel-coding"}
	codex := &requestPolicyTestExecutor{id: "codex"}
	manager.RegisterExecutor(bigmodel)
	manager.RegisterExecutor(codex)
	manager.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:    "bigmodel-coding",
				BaseURL: "https://bigmodel.example/v1",
				APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{
					{APIKey: "bigmodel-key"},
				},
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: "glm-5.1", Alias: "gpt-5.3-codex"},
				},
			},
		},
		RequestPolicies: []internalconfig.RequestPolicy{
			{
				Name: "glm-limit",
				Match: internalconfig.RequestPolicyMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
				Limits: internalconfig.RequestPolicyLimits{MaxRequestBytes: 10},
			},
		},
	})

	for _, auth := range []*Auth{
		{
			ID:       "bigmodel-auth",
			Provider: "bigmodel-coding",
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      "bigmodel-key",
				"base_url":     "https://bigmodel.example/v1",
				"provider_key": "bigmodel-coding",
				"compat_name":  "bigmodel-coding",
			},
		},
		{
			ID:       "codex-auth",
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "codex@example.com"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	resp, err := manager.Execute(context.Background(), []string{"bigmodel-coding", "codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","input":"large"}`),
	}, cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"gpt-5.3-codex","input":"large"}`),
		Metadata:        map[string]any{cliproxyexecutor.RequestBytesMetadataKey: 42},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("response = %s", resp.Payload)
	}
	if calls := bigmodel.Calls(); len(calls) != 0 {
		t.Fatalf("bigmodel calls = %v, want none", calls)
	}
	if calls := codex.Calls(); len(calls) != 1 || calls[0] != "codex-auth" {
		t.Fatalf("codex calls = %v, want [codex-auth]", calls)
	}
}

func TestManagerExecute_ProviderPreferencePrefersBigmodelAndFallsBack(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	bigmodel := &requestPolicyTestExecutor{id: "bigmodel-coding"}
	codex := &requestPolicyTestExecutor{id: "codex"}
	manager.RegisterExecutor(bigmodel)
	manager.RegisterExecutor(codex)
	manager.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:    "bigmodel-coding",
				BaseURL: "https://bigmodel.example/v1",
				APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{
					{APIKey: "bigmodel-key"},
				},
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: "glm-5.1", Alias: "gpt-5.3-codex"},
				},
			},
		},
		ProviderPreferences: []internalconfig.ProviderPreference{
			{
				Name: "prefer-bigmodel-coding",
				Match: internalconfig.ProviderPreferenceMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
				Priority: 100,
			},
		},
		RequestPolicies: []internalconfig.RequestPolicy{
			{
				Name: "glm-limit",
				Match: internalconfig.RequestPolicyMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
				Limits: internalconfig.RequestPolicyLimits{MaxRequestBytes: 10},
			},
		},
	})

	for _, auth := range []*Auth{
		{
			ID:       "bigmodel-auth",
			Provider: "bigmodel-coding",
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      "bigmodel-key",
				"base_url":     "https://bigmodel.example/v1",
				"provider_key": "bigmodel-coding",
				"compat_name":  "bigmodel-coding",
			},
		},
		{
			ID:       "codex-auth",
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "codex@example.com"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	_, err := manager.Execute(context.Background(), []string{"bigmodel-coding", "codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","input":"small"}`),
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestBytesMetadataKey: 5},
	})
	if err != nil {
		t.Fatalf("Execute() small error = %v", err)
	}
	if calls := bigmodel.Calls(); len(calls) != 1 || calls[0] != "bigmodel-auth" {
		t.Fatalf("bigmodel calls = %v, want [bigmodel-auth]", calls)
	}
	if calls := codex.Calls(); len(calls) != 0 {
		t.Fatalf("codex calls after small request = %v, want none", calls)
	}

	_, err = manager.Execute(context.Background(), []string{"bigmodel-coding", "codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","input":"large"}`),
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestBytesMetadataKey: 42},
	})
	if err != nil {
		t.Fatalf("Execute() large error = %v", err)
	}
	if calls := codex.Calls(); len(calls) != 1 || calls[0] != "codex-auth" {
		t.Fatalf("codex calls after fallback = %v, want [codex-auth]", calls)
	}
}

func TestManagerExecute_RequestPolicyRejectsWhenNoFallback(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	bigmodel := &requestPolicyTestExecutor{id: "bigmodel-coding"}
	manager.RegisterExecutor(bigmodel)
	manager.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name: "bigmodel-coding",
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: "glm-5.1", Alias: "gpt-5.3-codex"},
				},
			},
		},
		RequestPolicies: []internalconfig.RequestPolicy{
			{
				Name: "glm-limit",
				Match: internalconfig.RequestPolicyMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
				Limits:    internalconfig.RequestPolicyLimits{MaxRequestBytes: 10},
				OverLimit: internalconfig.RequestPolicyOverLimit{Action: "reject"},
			},
		},
	})

	auth := &Auth{
		ID:       "bigmodel-auth",
		Provider: "bigmodel-coding",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":      "bigmodel-key",
			"provider_key": "bigmodel-coding",
			"compat_name":  "bigmodel-coding",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	_, err := manager.Execute(context.Background(), []string{"bigmodel-coding"}, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex","input":"large"}`),
	}, cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"gpt-5.3-codex","input":"large"}`),
		Metadata:        map[string]any{cliproxyexecutor.RequestBytesMetadataKey: 42},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if statusErr, ok := err.(interface{ StatusCode() int }); !ok || statusErr.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("error status = %#v, want 413", err)
	}
	if calls := bigmodel.Calls(); len(calls) != 0 {
		t.Fatalf("bigmodel calls = %v, want none", calls)
	}
}

func TestManagerExecute_RequestPolicyFeatureSkipChannel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	bigmodel := &requestPolicyTestExecutor{id: "bigmodel-coding"}
	codex := &requestPolicyTestExecutor{id: "codex"}
	manager.RegisterExecutor(bigmodel)
	manager.RegisterExecutor(codex)
	manager.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name: "bigmodel-coding",
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: "glm-5.1", Alias: "gpt-5.3-codex"},
				},
			},
		},
		RequestPolicies: []internalconfig.RequestPolicy{
			{
				Name: "glm-multimodal-skip",
				Match: internalconfig.RequestPolicyMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
					RequestFeatures:   []string{"multimodal"},
				},
			},
		},
	})

	for _, auth := range []*Auth{
		{
			ID:       "bigmodel-auth",
			Provider: "bigmodel-coding",
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      "bigmodel-key",
				"provider_key": "bigmodel-coding",
				"compat_name":  "bigmodel-coding",
			},
		},
		{
			ID:       "codex-auth",
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "codex@example.com"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	_, err := manager.Execute(context.Background(), []string{"bigmodel-coding", "codex"}, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestFeaturesMetadataKey: []string{"multimodal"},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := bigmodel.Calls(); len(calls) != 0 {
		t.Fatalf("bigmodel calls = %v, want none", calls)
	}
	if calls := codex.Calls(); len(calls) != 1 || calls[0] != "codex-auth" {
		t.Fatalf("codex calls = %v, want [codex-auth]", calls)
	}
}
