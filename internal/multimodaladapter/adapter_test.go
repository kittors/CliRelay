package multimodaladapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func boolPtr(v bool) *bool { return &v }

func TestApplyHTTPExtractorOnlyMatchesConcreteRoute(t *testing.T) {
	cfg := config.MultimodalAdaptersConfig{
		Enabled:       boolPtr(true),
		DefaultAction: "extract",
		Rules: []config.MultimodalAdapterRule{
			{
				Name:      "glm-5.1-codex-vision",
				Extractor: "vision",
				Match: config.MultimodalAdapterMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
			},
		},
		Extractors: []config.MultimodalExtractorConfig{{Name: "vision", Type: "http", Endpoint: "http://127.0.0.1:1"}},
	}
	raw := []byte(`{"model":"gpt-5.3-codex","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`)

	out, report, err := Apply(context.Background(), raw, Route{
		RequestedModel:   "gpt-5.3-codex",
		UpstreamProvider: "codex",
		UpstreamModel:    "gpt-5.5",
		Protocol:         "openai-response",
	}, cfg)
	if err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if report.Applied {
		t.Fatalf("report.Applied = true, want false")
	}
	if string(out) != string(raw) {
		t.Fatalf("payload changed for unmatched route: %s", out)
	}
}

func TestApplyHTTPExtractorStripsMediaAndInjectsVisualContext(t *testing.T) {
	var extractorBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&extractorBody); err != nil {
			t.Fatalf("decode extractor body: %v", err)
		}
		_, _ = w.Write([]byte(`{"text":"The screenshot shows a terminal error: build failed."}`))
	}))
	defer server.Close()

	cfg := config.MultimodalAdaptersConfig{
		Enabled:       boolPtr(true),
		DefaultAction: "extract",
		InjectAs:      "visual_context",
		Rules: []config.MultimodalAdapterRule{
			{
				Name:      "glm-5.1-codex-vision",
				Extractor: "vision",
				Match: config.MultimodalAdapterMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
			},
		},
		Extractors: []config.MultimodalExtractorConfig{{Name: "vision", Type: "http", Endpoint: server.URL}},
	}
	raw := []byte(`{"model":"gpt-5.3-codex","input":[{"role":"user","content":[{"type":"input_text","text":"what failed?"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`)

	out, report, err := Apply(context.Background(), raw, Route{
		RequestedModel:   "gpt-5.3-codex",
		UpstreamProvider: "bigmodel-coding",
		UpstreamModel:    "glm-5.1",
		Protocol:         "openai-response",
	}, cfg)
	if err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if !report.Applied || !report.Stripped || !report.Injected || report.MediaItems != 1 || report.Extractor != "vision" {
		t.Fatalf("report = %#v", report)
	}
	body := string(out)
	if strings.Contains(body, "input_image") || strings.Contains(body, "image_url") {
		t.Fatalf("media was not stripped: %s", body)
	}
	if !strings.Contains(body, "visual_context") || !strings.Contains(body, "terminal error") {
		t.Fatalf("visual context was not injected: %s", body)
	}
	media, _ := extractorBody["media"].([]any)
	if len(media) != 1 {
		t.Fatalf("extractor media = %#v, want one item", extractorBody["media"])
	}
}

func TestApplyZAIVisionHTTPExtractorStripsMediaAndInjectsVisualContext(t *testing.T) {
	var extractorBody map[string]any
	var gotPath string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&extractorBody); err != nil {
			t.Fatalf("decode extractor body: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The screenshot shows error code E_CONN_RESET."}}]}`))
	}))
	defer server.Close()

	cfg := config.MultimodalAdaptersConfig{
		Enabled:       boolPtr(true),
		DefaultAction: "extract",
		InjectAs:      "visual_context",
		Rules: []config.MultimodalAdapterRule{
			{
				Name:      "glm-5.1-codex-vision",
				Extractor: "zai-vision-http",
				Match: config.MultimodalAdapterMatch{
					RequestedModels:   []string{"gpt-5.3-codex"},
					UpstreamProviders: []string{"bigmodel-coding"},
					UpstreamModels:    []string{"glm-5.1"},
				},
			},
		},
		Extractors: []config.MultimodalExtractorConfig{
			{
				Name:     "zai-vision-http",
				Type:     "zai-vision-http",
				Endpoint: server.URL + "/api/paas/v4",
				ToolName: "glm-4.5v",
				Headers:  map[string]string{"Authorization": "Bearer sk-test"},
			},
		},
	}
	raw := []byte(`{"model":"gpt-5.3-codex","input":[{"role":"user","content":[{"type":"input_text","text":"what failed?"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`)

	out, report, err := Apply(context.Background(), raw, Route{
		RequestedModel:   "gpt-5.3-codex",
		UpstreamProvider: "bigmodel-coding",
		UpstreamModel:    "glm-5.1",
		Protocol:         "openai-response",
	}, cfg)
	if err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if gotPath != "/api/paas/v4/chat/completions" {
		t.Fatalf("path = %q, want /api/paas/v4/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if extractorBody["model"] != "glm-4.5v" {
		t.Fatalf("model = %#v", extractorBody["model"])
	}
	if !report.Applied || !report.Stripped || !report.Injected || report.MediaItems != 1 || report.Extractor != "zai-vision-http" {
		t.Fatalf("report = %#v", report)
	}
	body := string(out)
	if strings.Contains(body, "input_image") || strings.Contains(body, "image_url") {
		t.Fatalf("media was not stripped: %s", body)
	}
	if !strings.Contains(body, "visual_context") || !strings.Contains(body, "E_CONN_RESET") {
		t.Fatalf("visual context was not injected: %s", body)
	}
}
