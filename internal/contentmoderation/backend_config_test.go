package contentmoderation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateProfileBackendRules(t *testing.T) {
	tests := []struct {
		name    string
		input   CreateProfileInput
		wantErr string
	}{
		{
			name:  "openai defaults fill endpoint",
			input: CreateProfileInput{Name: "openai", Mode: ModePreBlock, APIKey: "sk-test"},
		},
		{
			name:    "openai still requires an api key",
			input:   CreateProfileInput{Name: "openai", Mode: ModePreBlock},
			wantErr: "api_key is required",
		},
		{
			name:    "unknown backend rejected",
			input:   CreateProfileInput{Name: "x", Backend: "azure_content_safety"},
			wantErr: "backend must be",
		},
		{
			name:    "qwen3guard needs a base url in pre_block",
			input:   CreateProfileInput{Name: "guard", Mode: ModePreBlock, Backend: BackendQwen3Guard, Model: "qwen3guard"},
			wantErr: "base_url is required",
		},
		{
			name:    "qwen3guard needs a model in pre_block",
			input:   CreateProfileInput{Name: "guard", Mode: ModePreBlock, Backend: BackendQwen3Guard, BaseURL: "http://guard:8000"},
			wantErr: "model is required",
		},
		{
			name:  "qwen3guard does not require an api key",
			input: CreateProfileInput{Name: "guard", Mode: ModePreBlock, Backend: BackendQwen3Guard, BaseURL: "http://guard:8000", Model: "qwen3guard"},
		},
		{
			name:  "qwen3guard off may stay unconfigured",
			input: CreateProfileInput{Name: "guard", Mode: ModeOff, Backend: BackendQwen3Guard},
		},
		{
			name:  "qwen3guard keyword_only needs no endpoint",
			input: CreateProfileInput{Name: "guard", Mode: ModePreBlock, Backend: BackendQwen3Guard, KeywordMode: KeywordModeKeywordOnly},
		},
		{
			name:    "unknown scanner rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, Scanners: []string{"telepathy"}},
			wantErr: "unknown scanner category",
		},
		{
			name:    "unknown elevated category rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, ElevatedCategories: []string{"telepathy"}},
			wantErr: "unknown elevated category",
		},
		{
			name:    "unknown controversial action rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, ControversialAction: "warn"},
			wantErr: "controversial_action must be",
		},
		{
			name:    "input limit floor enforced",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, InputLimit: 8},
			wantErr: "input_limit must be",
		},
		{
			name:    "max chunks ceiling enforced",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, MaxChunks: MaxChunksLimit + 1},
			wantErr: "max_chunks must be",
		},
		{
			name:    "base url with credentials rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, BaseURL: "http://user:pass@guard:8000"},
			wantErr: "must not contain credentials",
		},
		{
			name:    "base url with query rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, BaseURL: "http://guard:8000?token=abc"},
			wantErr: "must not contain credentials",
		},
		{
			name:    "non-http scheme rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, BaseURL: "ftp://guard.internal:21"},
			wantErr: "must use http or https",
		},
		{
			name:    "hostless url rejected",
			input:   CreateProfileInput{Name: "guard", Backend: BackendQwen3Guard, BaseURL: "file:///etc/passwd"},
			wantErr: "invalid moderation base URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProfile("tenant-a", "id-1", tt.input, time.Now())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeModerationBaseURL(t *testing.T) {
	tests := []struct {
		raw, want string
		wantErr   bool
	}{
		{raw: "https://api.openai.com", want: "https://api.openai.com"},
		{raw: "https://api.openai.com/", want: "https://api.openai.com"},
		// A pasted OpenAI-style base must not produce /v1/v1/....
		{raw: "https://api.openai.com/v1", want: "https://api.openai.com"},
		{raw: "https://api.openai.com/V1/", want: "https://api.openai.com"},
		{raw: "http://guard.internal:8000/openai", want: "http://guard.internal:8000/openai"},
		// Private and loopback stay reachable: guard models are usually internal.
		{raw: "http://127.0.0.1:11434", want: "http://127.0.0.1:11434"},
		{raw: "http://10.0.0.5:8000", want: "http://10.0.0.5:8000"},
		{raw: "", wantErr: true},
		{raw: "guard.internal:8000", wantErr: true},
		{raw: "ftp://guard.internal", wantErr: true},
		{raw: "https://user@guard.internal", wantErr: true},
		{raw: "https://guard.internal#frag", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := normalizeModerationBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModerationEndpointURLJoinsPaths(t *testing.T) {
	got, err := moderationEndpointURL("https://api.openai.com/v1", "/v1/moderations")
	if err != nil || got != "https://api.openai.com/v1/moderations" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = moderationEndpointURL("http://guard:8000", "/v1/chat/completions")
	if err != nil || got != "http://guard:8000/v1/chat/completions" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// The size cap protects the hot path from an endpoint that never stops writing.
func TestOpenAIBackendRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("a", int(maxModerationResponseBytes)+16) + `","results":[{"category_scores":{"hate":1}}]}`))
	}))
	defer server.Close()
	profile := testProfile(t, "tenant-a", "api", ModePreBlock, KeywordModeAPIOnly)
	profile.BaseURL = server.URL
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "input")
	if decision.WouldBlock || decision.Action != ActionAPIError {
		t.Fatalf("decision = %#v, want fail-open", decision)
	}
	if got := moderationErrorClass(decision.ModerationError); got != "invalid_response" {
		t.Fatalf("error class = %q (%s)", got, decision.ModerationError)
	}
}

func TestNormalizeCategoryAliases(t *testing.T) {
	aliases := map[string]string{
		"Violent":                           ScannerViolent,
		"violence":                          ScannerViolent,
		"Non-violent Illegal Acts":          ScannerNonViolentIllegalActs,
		"non_violent_illegal_acts":          ScannerNonViolentIllegalActs,
		"Sexual Content or Sexual Acts":     ScannerSexualContentOrSexualActs,
		"sexual":                            ScannerSexualContentOrSexualActs,
		"PII":                               ScannerPII,
		"personal identifiable information": ScannerPII,
		"Suicide & Self-Harm":               ScannerSuicideAndSelfHarm,
		"suicide/self harm":                 ScannerSuicideAndSelfHarm,
		"Unethical Acts":                    ScannerUnethicalActs,
		"Politically Sensitive Topics":      ScannerPoliticallySensitive,
		"Copyright Violation":               ScannerCopyrightViolation,
		"Jailbreak":                         ScannerJailbreak,
		"prompt injection":                  ScannerJailbreak,
	}
	for alias, want := range aliases {
		if got := NormalizeCategory(alias); got != want {
			t.Fatalf("NormalizeCategory(%q) = %q, want %q", alias, got, want)
		}
	}
	if got := NormalizeCategory("Future Risk"); got != "future_risk" || IsScannerID(got) {
		t.Fatalf("unknown category handling = %q", got)
	}
}
