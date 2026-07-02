package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestPublicCcSwitchImportConfigsFiltersByAPIKeyPermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	if err := usage.ReplaceAllCcSwitchImportConfigs([]usage.CcSwitchImportConfigRow{
		{
			ID:                   "claude-1",
			ClientType:           "claude",
			ProviderName:         "Claude Code A",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"claude-code"},
			EndpointPath:         "/anthropic",
			UsageAutoInterval:    30,
			APIKeyField:          "ANTHROPIC_AUTH_TOKEN",
		},
		{
			ID:                   "claude-2",
			ClientType:           "claude",
			ProviderName:         "Claude Code B",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"claude-code"},
			EndpointPath:         "/anthropic",
			UsageAutoInterval:    30,
			APIKeyField:          "ANTHROPIC_AUTH_TOKEN",
		},
		{
			ID:                   "codex-1",
			ClientType:           "codex",
			ProviderName:         "Codex A",
			DefaultModel:         "gpt-5.2",
			AllowedChannelGroups: []string{"codex"},
			EndpointPath:         "/openai",
			UsageAutoInterval:    30,
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs: %v", err)
	}

	const apiKey = "sk-nyw1boql0aetf5o7dl2srrf9ciftgd95"
	if err := usage.ReplaceAllAPIKeys([]usage.APIKeyRow{
		{
			Key:                  apiKey,
			Name:                 "test",
			AllowedChannelGroups: []string{"claude-code"},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllAPIKeys: %v", err)
	}

	body := []byte(`{"api_key":"` + apiKey + `"}`)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/ccswitch-import-configs", bytes.NewReader(body))

	h := NewHandler(&config.Config{}, "", nil)
	h.GetPublicCcSwitchImportConfigs(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		Items  []usage.CcSwitchImportConfigRow `json:"items"`
		Found  bool                            `json:"found"`
		APIKey string                          `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !got.Found {
		t.Fatalf("found = false, want true")
	}
	if got.APIKey == apiKey || got.APIKey == "" {
		t.Fatalf("api_key should be masked, got %q", got.APIKey)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(got.Items))
	}
	if got.Items[0].ClientType != "claude" || got.Items[1].ClientType != "claude" {
		t.Fatalf("items = %#v, want claude only", got.Items)
	}
}

func TestPublicCcSwitchImportConfigsReturnsEmptyWhenKeyUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/ccswitch-import-configs", bytes.NewReader([]byte(`{"api_key":"sk-unknown"}`)))

	h := NewHandler(&config.Config{}, "", nil)
	h.GetPublicCcSwitchImportConfigs(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		Items []usage.CcSwitchImportConfigRow `json:"items"`
		Found bool                            `json:"found"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Found {
		t.Fatalf("found = true, want false")
	}
	if len(got.Items) != 0 {
		t.Fatalf("items len = %d, want 0", len(got.Items))
	}
}

func TestPublicCcSwitchImportConfigsIncludesCodexModelCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	if err := usage.ReplaceAllCcSwitchImportConfigs([]usage.CcSwitchImportConfigRow{
		{
			ID:                   "codex-deepseek",
			ClientType:           "codex",
			ProviderName:         "pro pool + deepseek",
			DefaultModel:         "gpt-5.5",
			AllowedChannelGroups: []string{"codex"},
			EndpointPath:         "/v1",
			UsageAutoInterval:    30,
			ModelMappings: []usage.CcSwitchModelMappingRow{
				{RequestModel: "gpt-5.5", TargetModel: "gpt-5.5"},
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
				{RequestModel: "deepseek-v4-pro", TargetModel: "deepseek-reasoner"},
			},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs: %v", err)
	}

	const apiKey = "sk-public-catalog-test"
	if err := usage.ReplaceAllAPIKeys([]usage.APIKeyRow{
		{
			Key:                  apiKey,
			Name:                 "catalog test",
			AllowedChannelGroups: []string{"codex"},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllAPIKeys: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/ccswitch-import-configs", bytes.NewReader([]byte(`{"api_key":"`+apiKey+`"}`)))

	h := NewHandler(&config.Config{}, "", nil)
	h.GetPublicCcSwitchImportConfigs(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		Items []usage.CcSwitchImportConfigRow `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(got.Items))
	}

	item := got.Items[0]
	if item.CodexModelCatalogFilename != usage.CcSwitchCodexModelCatalogFilename {
		t.Fatalf("codex catalog filename = %q, want %q", item.CodexModelCatalogFilename, usage.CcSwitchCodexModelCatalogFilename)
	}
	if item.CodexModelCatalog == nil {
		t.Fatal("codex model catalog = nil, want generated catalog")
	}

	slugs := make([]string, 0, len(item.CodexModelCatalog.Models))
	for _, model := range item.CodexModelCatalog.Models {
		slugs = append(slugs, model.Slug)
	}
	want := []string{"gpt-5.5", "deepseek-v4-flash", "deepseek-v4-pro"}
	if len(slugs) != len(want) {
		t.Fatalf("catalog slugs len = %d, want %d: %#v", len(slugs), len(want), slugs)
	}
	for idx := range want {
		if slugs[idx] != want[idx] {
			t.Fatalf("catalog slug[%d] = %q, want %q; all=%#v", idx, slugs[idx], want[idx], slugs)
		}
	}
}
