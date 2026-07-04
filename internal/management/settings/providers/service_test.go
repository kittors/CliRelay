package providers

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestReplaceOpenAICompatibilityNormalizesFiltersAndRollsBack(t *testing.T) {
	validationErr := errors.New("duplicate channel")
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "existing",
			BaseURL: "https://old.example",
		}},
	}

	err := NewService(cfg, func() error { return validationErr }).ReplaceOpenAICompatibility([]config.OpenAICompatibility{{
		Name:    " next ",
		BaseURL: " https://next.example ",
	}})
	if !errors.Is(err, validationErr) {
		t.Fatalf("ReplaceOpenAICompatibility() error = %v, want validation error", err)
	}
	if got := cfg.OpenAICompatibility; len(got) != 1 || got[0].Name != "existing" {
		t.Fatalf("OpenAICompatibility after rollback = %#v, want existing entry", got)
	}

	err = NewService(cfg, nil).ReplaceOpenAICompatibility([]config.OpenAICompatibility{
		{
			Name:    " next ",
			BaseURL: " https://next.example ",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
				APIKey:   " key-1 ",
				ProxyURL: " http://proxy.example ",
				ProxyID:  " proxy-a ",
			}},
			Headers: map[string]string{" x-trace ": " on "},
		},
		{
			Name:    "blank",
			BaseURL: " ",
		},
	})
	if err != nil {
		t.Fatalf("ReplaceOpenAICompatibility() error = %v, want nil", err)
	}
	got := cfg.OpenAICompatibility
	if len(got) != 1 {
		t.Fatalf("OpenAICompatibility len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Name != "next" || got[0].BaseURL != "https://next.example" {
		t.Fatalf("normalized entry = %#v, want trimmed values", got[0])
	}
	if got[0].APIKeyEntries[0].APIKey != "key-1" || got[0].APIKeyEntries[0].ProxyURL != "http://proxy.example" || got[0].APIKeyEntries[0].ProxyID != "proxy-a" {
		t.Fatalf("normalized api key entry = %#v", got[0].APIKeyEntries[0])
	}
	if _, ok := got[0].Headers["x-trace"]; !ok {
		t.Fatalf("headers = %#v, want normalized x-trace header", got[0].Headers)
	}
}

func TestPatchOpenAICompatibilityUpdatesAndDeletes(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "compat",
		BaseURL: "https://old.example",
	}}}
	name := " compat "
	newName := " renamed "
	disabled := true
	baseURL := " https://new.example "
	models := []config.OpenAICompatibilityModel{{Name: " gpt-4.1 ", Alias: " smart "}}

	err := NewService(cfg, nil).PatchOpenAICompatibility(nil, &name, OpenAICompatibilityPatch{
		Name:     &newName,
		Disabled: &disabled,
		BaseURL:  &baseURL,
		Models:   &models,
	})
	if err != nil {
		t.Fatalf("PatchOpenAICompatibility() error = %v, want nil", err)
	}
	if got := cfg.OpenAICompatibility[0]; got.Name != "renamed" || got.BaseURL != "https://new.example" || !got.Disabled {
		t.Fatalf("patched entry = %#v, want trimmed updated entry", got)
	}

	index := 0
	emptyBaseURL := " "
	err = NewService(cfg, nil).PatchOpenAICompatibility(&index, nil, OpenAICompatibilityPatch{BaseURL: &emptyBaseURL})
	if err != nil {
		t.Fatalf("PatchOpenAICompatibility(delete) error = %v, want nil", err)
	}
	if len(cfg.OpenAICompatibility) != 0 {
		t.Fatalf("OpenAICompatibility after delete = %#v, want empty", cfg.OpenAICompatibility)
	}
}

func TestVertexCompatKeysNormalizePatchAndDelete(t *testing.T) {
	cfg := &config.Config{}
	svc := NewService(cfg, nil)

	svc.ReplaceVertexCompatKeys([]config.VertexCompatKey{{
		APIKey:  " vertex-key ",
		BaseURL: " https://vertex.example ",
		Headers: map[string]string{
			" x-trace ": " on ",
		},
		Models: []config.VertexCompatModel{
			{Name: " gemini-pro ", Alias: " pro "},
			{Name: " ", Alias: "drop"},
		},
	}})

	if len(cfg.VertexCompatAPIKey) != 1 {
		t.Fatalf("VertexCompatAPIKey len = %d, want 1", len(cfg.VertexCompatAPIKey))
	}
	got := cfg.VertexCompatAPIKey[0]
	if got.APIKey != "vertex-key" || got.BaseURL != "https://vertex.example" {
		t.Fatalf("normalized vertex entry = %#v", got)
	}
	if !reflect.DeepEqual(got.Models, []config.VertexCompatModel{{Name: "gemini-pro", Alias: "pro"}}) {
		t.Fatalf("normalized models = %#v, want one trimmed model", got.Models)
	}

	match := " vertex-key "
	proxyURL := " http://proxy.example "
	err := svc.PatchVertexCompatKey(nil, &match, VertexCompatPatch{ProxyURL: &proxyURL})
	if err != nil {
		t.Fatalf("PatchVertexCompatKey() error = %v, want nil", err)
	}
	if cfg.VertexCompatAPIKey[0].ProxyURL != "http://proxy.example" {
		t.Fatalf("ProxyURL = %q, want trimmed proxy URL", cfg.VertexCompatAPIKey[0].ProxyURL)
	}

	index := 0
	emptyAPIKey := " "
	err = svc.PatchVertexCompatKey(&index, nil, VertexCompatPatch{APIKey: &emptyAPIKey})
	if err != nil {
		t.Fatalf("PatchVertexCompatKey(delete) error = %v, want nil", err)
	}
	if len(cfg.VertexCompatAPIKey) != 0 {
		t.Fatalf("VertexCompatAPIKey after delete = %#v, want empty", cfg.VertexCompatAPIKey)
	}
}

func TestGeminiKeysReplacePatchDeleteAndRollback(t *testing.T) {
	validationErr := errors.New("invalid channel")
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "existing"}},
	}
	svc := NewService(cfg, func() error { return validationErr })

	err := svc.ReplaceGeminiKeys([]config.GeminiKey{{APIKey: " next "}})
	if !errors.Is(err, validationErr) {
		t.Fatalf("ReplaceGeminiKeys() error = %v, want validation error", err)
	}
	if got := cfg.GeminiKey; len(got) != 1 || got[0].APIKey != "existing" {
		t.Fatalf("GeminiKey after rollback = %#v, want existing entry", got)
	}

	svc = NewService(cfg, nil)
	err = svc.ReplaceGeminiKeys([]config.GeminiKey{{APIKey: " next ", Prefix: " /team/ ", ProxyURL: " http://proxy.example "}})
	if err != nil {
		t.Fatalf("ReplaceGeminiKeys() error = %v, want nil", err)
	}
	if got := cfg.GeminiKey[0]; got.APIKey != "next" || got.Prefix != "team" || got.ProxyURL != "http://proxy.example" {
		t.Fatalf("normalized gemini key = %#v", got)
	}

	match := " next "
	emptyAPIKey := " "
	err = svc.PatchGeminiKey(nil, &match, GeminiKeyPatch{APIKey: &emptyAPIKey})
	if err != nil {
		t.Fatalf("PatchGeminiKey(delete) error = %v, want nil", err)
	}
	if len(cfg.GeminiKey) != 0 {
		t.Fatalf("GeminiKey after delete = %#v, want empty", cfg.GeminiKey)
	}
}

func TestClaudeKeysReplacePatchAndDelete(t *testing.T) {
	cfg := &config.Config{}
	svc := NewService(cfg, nil)

	err := svc.ReplaceClaudeKeys([]config.ClaudeKey{
		{Name: "oauth-row", APIKey: ""},
		{
			Name:    " claude ",
			APIKey:  " sk-claude ",
			BaseURL: " https://claude.example ",
			Models:  []config.ClaudeModel{{Name: " claude-sonnet-4 ", Alias: " sonnet "}},
		},
	})
	if err != nil {
		t.Fatalf("ReplaceClaudeKeys() error = %v, want nil", err)
	}
	if len(cfg.ClaudeKey) != 1 {
		t.Fatalf("ClaudeKey len = %d, want 1", len(cfg.ClaudeKey))
	}
	if got := cfg.ClaudeKey[0]; got.Name != "claude" || got.APIKey != "sk-claude" || got.BaseURL != "https://claude.example" {
		t.Fatalf("normalized claude key = %#v", got)
	}

	match := "sk-claude"
	blankAPIKey := " "
	err = svc.PatchClaudeKey(nil, &match, ClaudeKeyPatch{APIKey: &blankAPIKey})
	if err != nil {
		t.Fatalf("PatchClaudeKey(blank api key) error = %v, want nil", err)
	}
	if len(cfg.ClaudeKey) != 0 {
		t.Fatalf("ClaudeKey after blank api key patch = %#v, want empty", cfg.ClaudeKey)
	}
}

func TestCodexKeysReplacePatchDeleteAndRollback(t *testing.T) {
	validationErr := errors.New("channel conflict")
	cfg := &config.Config{
		CodexKey: []config.CodexKey{{APIKey: "existing", BaseURL: "https://old.example"}},
	}
	svc := NewService(cfg, func() error { return validationErr })

	err := svc.ReplaceCodexKeys([]config.CodexKey{{APIKey: "next", BaseURL: "https://new.example"}})
	if !errors.Is(err, validationErr) {
		t.Fatalf("ReplaceCodexKeys() error = %v, want validation error", err)
	}
	if got := cfg.CodexKey; len(got) != 1 || got[0].APIKey != "existing" {
		t.Fatalf("CodexKey after rollback = %#v, want existing entry", got)
	}

	svc = NewService(cfg, nil)
	err = svc.ReplaceCodexKeys([]config.CodexKey{
		{APIKey: "next", BaseURL: " https://codex.example ", ProxyURL: " http://proxy.example "},
		{APIKey: "drop", BaseURL: " "},
	})
	if err != nil {
		t.Fatalf("ReplaceCodexKeys() error = %v, want nil", err)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("CodexKey len = %d, want 1", len(cfg.CodexKey))
	}
	if got := cfg.CodexKey[0]; got.BaseURL != "https://codex.example" || got.ProxyURL != "http://proxy.example" {
		t.Fatalf("normalized codex key = %#v", got)
	}

	match := "next"
	emptyBaseURL := " "
	err = svc.PatchCodexKey(nil, &match, CodexKeyPatch{BaseURL: &emptyBaseURL})
	if err != nil {
		t.Fatalf("PatchCodexKey(delete) error = %v, want nil", err)
	}
	if len(cfg.CodexKey) != 0 {
		t.Fatalf("CodexKey after delete = %#v, want empty", cfg.CodexKey)
	}
}

func TestBedrockKeysReplacePatchAndDelete(t *testing.T) {
	cfg := &config.Config{}
	svc := NewService(cfg, nil)

	err := svc.ReplaceBedrockKeys([]config.BedrockKey{
		{
			Name:            " aws api ",
			AuthMode:        "api-key",
			APIKey:          " br-key ",
			Region:          " eu-west-1 ",
			SecretAccessKey: "should-trim",
			Models:          []config.BedrockModel{{Name: " claude-sonnet-4-5 ", Alias: " aws-sonnet "}},
		},
		{
			Name:            " aws sigv4 ",
			AuthMode:        "sigv4",
			AccessKeyID:     " AKIA ",
			SecretAccessKey: " SECRET ",
			Region:          " us-east-1 ",
		},
	})
	if err != nil {
		t.Fatalf("ReplaceBedrockKeys() error = %v, want nil", err)
	}
	if len(cfg.BedrockKey) != 2 {
		t.Fatalf("BedrockKey len = %d, want 2", len(cfg.BedrockKey))
	}
	if got := cfg.BedrockKey[0]; got.Name != "aws api" || got.APIKey != "br-key" || got.Region != "eu-west-1" {
		t.Fatalf("normalized bedrock key = %#v", got)
	}

	match := "AKIA"
	renamed := "renamed sigv4"
	region := "ap-southeast-2"
	sessionToken := "SESSION"
	err = svc.PatchBedrockKey(nil, &match, BedrockKeyPatch{
		Name:         &renamed,
		Region:       &region,
		SessionToken: &sessionToken,
	})
	if err != nil {
		t.Fatalf("PatchBedrockKey() error = %v, want nil", err)
	}
	if got := cfg.BedrockKey[1]; got.Name != "renamed sigv4" || got.Region != "ap-southeast-2" || got.SessionToken != "SESSION" {
		t.Fatalf("patched bedrock key = %#v", got)
	}

	if !svc.DeleteBedrockKeyByIndex(0) {
		t.Fatal("DeleteBedrockKeyByIndex() = false, want true")
	}
	if len(cfg.BedrockKey) != 1 || cfg.BedrockKey[0].Name != "renamed sigv4" {
		t.Fatalf("BedrockKey after delete = %#v", cfg.BedrockKey)
	}
}

func TestOpenCodeGoKeysReplacePatchDeleteAndRollback(t *testing.T) {
	validationErr := errors.New("channel conflict")
	cfg := &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "existing"}},
	}
	svc := NewService(cfg, func() error { return validationErr })

	err := svc.ReplaceOpenCodeGoKeys([]config.OpenCodeGoKey{{APIKey: " go-key "}})
	if !errors.Is(err, validationErr) {
		t.Fatalf("ReplaceOpenCodeGoKeys() error = %v, want validation error", err)
	}
	if got := cfg.OpenCodeGoKey; len(got) != 1 || got[0].APIKey != "existing" {
		t.Fatalf("OpenCodeGoKey after rollback = %#v, want existing entry", got)
	}

	svc = NewService(cfg, nil)
	err = svc.ReplaceOpenCodeGoKeys([]config.OpenCodeGoKey{{
		APIKey:              " go-key ",
		Name:                " primary ",
		Prefix:              " team ",
		Headers:             map[string]string{"X-Test": " yes "},
		VisionFallbackModel: " qwen3.5-plus ",
		WorkspaceID:         " wrk_123 ",
		AuthCookie:          " auth-token ",
	}})
	if err != nil {
		t.Fatalf("ReplaceOpenCodeGoKeys() error = %v, want nil", err)
	}
	if len(cfg.OpenCodeGoKey) != 1 {
		t.Fatalf("OpenCodeGoKey len = %d, want 1", len(cfg.OpenCodeGoKey))
	}
	if got := cfg.OpenCodeGoKey[0]; got.APIKey != "go-key" || got.Prefix != "team" || got.WorkspaceID != "wrk_123" || got.AuthCookie != "auth-token" {
		t.Fatalf("normalized opencode go key = %#v", got)
	}

	index := 0
	name := "secondary"
	excludedModels := []string{" minimax-m2.5 "}
	visionFallback := " qwen3.6-plus "
	workspaceID := " https://opencode.ai/workspace/wrk_456/go "
	authCookie := " auth-next "
	err = svc.PatchOpenCodeGoKey(&index, nil, nil, OpenCodeGoPatch{
		Name:           &name,
		ExcludedModels: &excludedModels,
		VisionFallback: &visionFallback,
		WorkspaceID:    &workspaceID,
		AuthCookie:     &authCookie,
	})
	if err != nil {
		t.Fatalf("PatchOpenCodeGoKey() error = %v, want nil", err)
	}
	if got := cfg.OpenCodeGoKey[0]; got.Name != "secondary" || !reflect.DeepEqual(got.ExcludedModels, []string{"minimax-m2.5"}) || got.WorkspaceID != "wrk_456" || got.AuthCookie != "auth-next" {
		t.Fatalf("patched opencode go key = %#v", got)
	}

	if !svc.DeleteOpenCodeGoKeyByName("secondary") {
		t.Fatal("DeleteOpenCodeGoKeyByName() = false, want true")
	}
	if len(cfg.OpenCodeGoKey) != 0 {
		t.Fatalf("OpenCodeGoKey after delete = %#v, want empty", cfg.OpenCodeGoKey)
	}
}

func TestOpenCodeGoKeysRejectClinePassModels(t *testing.T) {
	cfg := &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{
			APIKey: "existing",
			Models: []config.OpenCodeGoModel{{
				Name: "glm-5.2",
			}},
		}},
	}
	svc := NewService(cfg, nil)

	err := svc.ReplaceOpenCodeGoKeys([]config.OpenCodeGoKey{{
		APIKey: "go-key",
		Models: []config.OpenCodeGoModel{{
			Name: " cline-pass/glm-5.2 ",
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "opencode-go models contains invalid model") {
		t.Fatalf("ReplaceOpenCodeGoKeys() error = %v, want invalid opencode-go model error", err)
	}
	if got := cfg.OpenCodeGoKey; len(got) != 1 || got[0].APIKey != "existing" || got[0].Models[0].Name != "glm-5.2" {
		t.Fatalf("OpenCodeGoKey after rejected replace = %#v, want unchanged existing entry", got)
	}

	badExcluded := []string{"cline-pass/minimax-m3"}
	err = svc.PatchOpenCodeGoKey(nil, stringPtr("existing"), nil, OpenCodeGoPatch{ExcludedModels: &badExcluded})
	if err == nil || !strings.Contains(err.Error(), "opencode-go excluded-models contains invalid model") {
		t.Fatalf("PatchOpenCodeGoKey(excluded-models) error = %v, want invalid opencode-go model error", err)
	}
	if got := cfg.OpenCodeGoKey[0].ExcludedModels; len(got) != 0 {
		t.Fatalf("OpenCodeGoKey excluded models after rejected patch = %#v, want unchanged empty list", got)
	}

	validExcluded := []string{"*"}
	validFallback := " qwen3.5-plus "
	validModels := []config.OpenCodeGoModel{{Name: " glm-5.2 "}}
	err = svc.PatchOpenCodeGoKey(nil, stringPtr("existing"), nil, OpenCodeGoPatch{
		Models:         &validModels,
		ExcludedModels: &validExcluded,
		VisionFallback: &validFallback,
	})
	if err != nil {
		t.Fatalf("PatchOpenCodeGoKey(valid models) error = %v, want nil", err)
	}
	if got := cfg.OpenCodeGoKey[0]; got.Models[0].Name != "glm-5.2" || got.ExcludedModels[0] != "*" || got.VisionFallbackModel != "qwen3.5-plus" {
		t.Fatalf("OpenCodeGoKey after valid patch = %#v", got)
	}
}

func TestClineKeysRejectNonClinePassModels(t *testing.T) {
	cfg := &config.Config{
		ClineKey: []config.ClineKey{{
			APIKey: "existing",
			Models: []config.ClineModel{{
				Name: "cline-pass/glm-5.2",
			}},
		}},
	}
	svc := NewService(cfg, nil)

	err := svc.ReplaceClineKeys([]config.ClineKey{{
		APIKey: "cline-key",
		Models: []config.ClineModel{{
			Name: " glm-5.2 ",
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "cline models contains invalid model") {
		t.Fatalf("ReplaceClineKeys() error = %v, want invalid cline model error", err)
	}
	if got := cfg.ClineKey; len(got) != 1 || got[0].APIKey != "existing" || got[0].Models[0].Name != "cline-pass/glm-5.2" {
		t.Fatalf("ClineKey after rejected replace = %#v, want unchanged existing entry", got)
	}

	badFallback := "qwen3.5-plus"
	err = svc.PatchClineKey(nil, stringPtr("existing"), nil, ClinePatch{VisionFallback: &badFallback})
	if err == nil || !strings.Contains(err.Error(), "cline vision-fallback-model contains invalid model") {
		t.Fatalf("PatchClineKey(vision-fallback-model) error = %v, want invalid cline model error", err)
	}
	if got := cfg.ClineKey[0].VisionFallbackModel; got != "" {
		t.Fatalf("ClineKey vision fallback after rejected patch = %q, want unchanged empty value", got)
	}

	validExcluded := []string{"*", " cline-pass/minimax-m3 "}
	validFallback := " cline-pass/mimo-v2.5-pro "
	validModels := []config.ClineModel{{Name: " cline-pass/qwen3.7-max "}}
	err = svc.PatchClineKey(nil, stringPtr("existing"), nil, ClinePatch{
		Models:         &validModels,
		ExcludedModels: &validExcluded,
		VisionFallback: &validFallback,
	})
	if err != nil {
		t.Fatalf("PatchClineKey(valid models) error = %v, want nil", err)
	}
	if got := cfg.ClineKey[0]; got.Models[0].Name != "cline-pass/qwen3.7-max" || got.ExcludedModels[0] != "*" || got.ExcludedModels[1] != "cline-pass/minimax-m3" || got.VisionFallbackModel != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("ClineKey after valid patch = %#v", got)
	}
}

func stringPtr(value string) *string {
	return &value
}
