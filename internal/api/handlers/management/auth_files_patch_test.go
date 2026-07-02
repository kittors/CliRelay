package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type failingAuthStore struct {
	items map[string]*coreauth.Auth
}

func (s *failingAuthStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	_ = ctx
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func (s *failingAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	_ = ctx
	_ = auth
	return "", errors.New("persist failed")
}

func (s *failingAuthStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	_ = id
	return nil
}

func TestPatchAuthFileFieldsUpdatesOAuthChannelLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-1",
		FileName: "oauth-auth-1.json",
		Provider: "claude",
		Metadata: map[string]any{
			"email": "old@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "oauth-auth-1.json",
		"label": "Team Alpha",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-1")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if updated.Label != "Team Alpha" {
		t.Fatalf("label = %q, want %q", updated.Label, "Team Alpha")
	}
	if got, _ := updated.Metadata["label"].(string); got != "Team Alpha" {
		t.Fatalf("metadata label = %q, want %q", got, "Team Alpha")
	}
}

func TestPatchAuthFileFieldsUpdatesCodexOAuthAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-oauth-1",
		FileName: "codex-oauth-1.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":                           "codex-oauth-1.json",
		"codex_cli_only":                 true,
		"codex_cli_only_allowed_clients": []string{"claude_code", "CLAUDE_CODE"},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID("codex-oauth-1")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if got, _ := updated.Metadata["codex_cli_only"].(bool); !got {
		t.Fatalf("codex_cli_only = %#v, want true", updated.Metadata["codex_cli_only"])
	}
	allowed, ok := updated.Metadata["codex_cli_only_allowed_clients"].([]string)
	if !ok || len(allowed) != 1 || allowed[0] != codexadmission.AllowedClientClaudeCode {
		t.Fatalf("allowed clients = %#v, want [claude_code]", updated.Metadata["codex_cli_only_allowed_clients"])
	}
}

func TestPatchAuthFileFieldsUpdatesKimiOAuthChannelLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-auth-1",
		FileName: "kimi-auth-1.json",
		Provider: "kimi",
		Label:    "Kimi User",
		Metadata: map[string]any{
			"type":          "kimi",
			"access_token":  "kimi-access-token",
			"refresh_token": "kimi-refresh-token",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "kimi-auth-1.json",
		"label": "Kimi Team A",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("kimi-auth-1")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if updated.Label != "Kimi Team A" {
		t.Fatalf("label = %q, want %q", updated.Label, "Kimi Team A")
	}
	if got, _ := updated.Metadata["label"].(string); got != "Kimi Team A" {
		t.Fatalf("metadata label = %q, want %q", got, "Kimi Team A")
	}
}

func TestPatchAuthFileFieldsUpdatesCustomTagsAndHiddenDefaultTags(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-tags",
		FileName: "oauth-auth-tags.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":     "tags@example.com",
			"plan_type": "pro",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":                "oauth-auth-tags.json",
		"custom_tags":         []string{" Team ", "priority", "team"},
		"hidden_default_tags": []string{"pro", " codex "},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-tags")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	customTags, ok := updated.Metadata["custom_tags"].([]string)
	if !ok {
		t.Fatalf("custom_tags type = %T, want []string", updated.Metadata["custom_tags"])
	}
	if len(customTags) != 2 || customTags[0] != "team" || customTags[1] != "priority" {
		t.Fatalf("custom_tags = %#v, want [team priority]", customTags)
	}
	hiddenTags, ok := updated.Metadata["hidden_default_tags"].([]string)
	if !ok {
		t.Fatalf("hidden_default_tags type = %T, want []string", updated.Metadata["hidden_default_tags"])
	}
	if len(hiddenTags) != 2 || hiddenTags[0] != "pro" || hiddenTags[1] != "codex" {
		t.Fatalf("hidden_default_tags = %#v, want [pro codex]", hiddenTags)
	}
}

func TestPatchAuthFileFieldsUpdatesDisplayTags(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-display-tags",
		FileName: "oauth-auth-display-tags.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":     "display-tags@example.com",
			"plan_type": "pro",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":         "oauth-auth-display-tags.json",
		"custom_tags":  []string{"vip"},
		"display_tags": []string{"codex", "vip"},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-display-tags")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	displayTags, ok := updated.Metadata["display_tags"].([]string)
	if !ok {
		t.Fatalf("display_tags type = %T, want []string", updated.Metadata["display_tags"])
	}
	if len(displayTags) != 2 || displayTags[0] != "codex" || displayTags[1] != "vip" {
		t.Fatalf("display_tags = %#v, want [codex vip]", displayTags)
	}
}

func TestPatchAuthFileFieldsRejectsMoreThanThreeCustomTags(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-too-many-tags",
		FileName: "oauth-auth-too-many-tags.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "tags@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":        "oauth-auth-too-many-tags.json",
		"custom_tags": []string{"one", "two", "three", "four"},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestBuildAuthFileEntryIncludesDefaultAndDisplayTags(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "codex-tagged-auth",
		FileName: "codex-tagged-auth.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-tagged-auth.json",
		},
		Metadata: map[string]any{
			"email":               "tagged@example.com",
			"plan_type":           "pro",
			"custom_tags":         []string{"team-a"},
			"hidden_default_tags": []string{"pro"},
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	defaultTags, ok := entry["default_tags"].([]string)
	if !ok {
		t.Fatalf("default_tags type = %T, want []string", entry["default_tags"])
	}
	if len(defaultTags) != 2 || defaultTags[0] != "codex" || defaultTags[1] != "pro" {
		t.Fatalf("default_tags = %#v, want [codex pro]", defaultTags)
	}
	displayTags, ok := entry["display_tags"].([]string)
	if !ok {
		t.Fatalf("display_tags type = %T, want []string", entry["display_tags"])
	}
	if len(displayTags) != 2 || displayTags[0] != "codex" || displayTags[1] != "team-a" {
		t.Fatalf("display_tags = %#v, want [codex team-a]", displayTags)
	}
}

func TestBuildAuthFileEntryHonorsExplicitEmptyDisplayTags(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "codex-hidden-tags",
		FileName: "codex-hidden-tags.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-hidden-tags.json",
		},
		Metadata: map[string]any{
			"plan_type":    "pro",
			"custom_tags":  []string{"vip"},
			"display_tags": []string{},
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	displayTags, ok := entry["display_tags"].([]string)
	if !ok {
		t.Fatalf("display_tags type = %T, want []string", entry["display_tags"])
	}
	if len(displayTags) != 0 {
		t.Fatalf("display_tags = %#v, want empty list", displayTags)
	}
}

func TestBuildAuthFileEntryReplacesStaleExplicitPlanDisplayTag(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "codex-downgraded-tags",
		FileName: "codex-downgraded-tags.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-downgraded-tags.json",
		},
		Metadata: map[string]any{
			"plan_type":    "free",
			"display_tags": []string{"codex", "plus"},
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	defaultTags, ok := entry["default_tags"].([]string)
	if !ok {
		t.Fatalf("default_tags type = %T, want []string", entry["default_tags"])
	}
	if len(defaultTags) != 2 || defaultTags[0] != "codex" || defaultTags[1] != "free" {
		t.Fatalf("default_tags = %#v, want [codex free]", defaultTags)
	}
	displayTags, ok := entry["display_tags"].([]string)
	if !ok {
		t.Fatalf("display_tags type = %T, want []string", entry["display_tags"])
	}
	if len(displayTags) != 2 || displayTags[0] != "codex" || displayTags[1] != "free" {
		t.Fatalf("display_tags = %#v, want [codex free]", displayTags)
	}
}

func TestBuildAuthFileEntryExposesMetadataPlanTypeBeforeIDTokenClaim(t *testing.T) {
	idToken := makeManagementJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "plus",
			"chatgpt_account_id": "acct_123",
		},
	})
	auth := &coreauth.Auth{
		ID:       "codex-stale-id-token",
		FileName: "codex-stale-id-token.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-stale-id-token.json",
		},
		Metadata: map[string]any{
			"plan_type": "free",
			"id_token":  idToken,
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	if got, _ := entry["plan_type"].(string); got != "free" {
		t.Fatalf("plan_type = %q, want free", got)
	}
	claims, ok := entry["id_token"].(map[string]any)
	if !ok {
		t.Fatalf("id_token type = %T, want map[string]any", entry["id_token"])
	}
	if got, _ := claims["plan_type"].(string); got != "plus" {
		t.Fatalf("id_token.plan_type = %q, want plus", got)
	}
}

func makeManagementJWTForTest(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}

func TestBuildAuthFileEntryIncludesActiveRestrictions(t *testing.T) {
	nextRetry := time.Now().Add(34*time.Minute + 50*time.Second).UTC().Truncate(time.Second)
	auth := &coreauth.Auth{
		ID:       "codex-restricted",
		FileName: "codex-restricted.json",
		Provider: "codex",
		Status:   coreauth.StatusError,
		Attributes: map[string]string{
			"path": "codex-restricted.json",
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Status:         coreauth.StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				LastError:      &coreauth.Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
			},
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	restrictions, ok := entry["restrictions"].([]map[string]any)
	if !ok {
		t.Fatalf("restrictions type = %T, want []map[string]any", entry["restrictions"])
	}
	if len(restrictions) != 1 {
		t.Fatalf("restrictions length = %d, want 1", len(restrictions))
	}
	got := restrictions[0]
	if got["scope"] != "model" || got["model"] != "gpt-5" || got["http_status"] != http.StatusUnauthorized {
		t.Fatalf("restriction = %#v, want model gpt-5 401", got)
	}
	if got["status_message"] != "unauthorized" {
		t.Fatalf("status_message = %#v, want unauthorized", got["status_message"])
	}
	if retry, ok := got["next_retry_after"].(time.Time); !ok || !retry.Equal(nextRetry) {
		t.Fatalf("next_retry_after = %#v, want %v", got["next_retry_after"], nextRetry)
	}
}

func TestBuildAuthFileEntryDedupesDuplicateRestrictions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	nextRetry := now.Add(25 * time.Minute)
	nextRecover := now.Add(25 * time.Minute)

	makeModelState := func() *coreauth.ModelState {
		return &coreauth.ModelState{
			Status:         coreauth.StatusError,
			StatusMessage:  `{"error":{"type":"usage_limit_reached"}}`,
			Unavailable:    true,
			NextRetryAfter: nextRetry,
			LastError:      &coreauth.Error{Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
			Quota: coreauth.QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: nextRecover,
			},
		}
	}

	auth := &coreauth.Auth{
		ID:       "codex-429-dedupe",
		FileName: "codex-429-dedupe.json",
		Provider: "codex",
		Status:   coreauth.StatusError,
		Attributes: map[string]string{
			"path": "codex-429-dedupe.json",
		},
		StatusMessage:  `{"error":{"type":"usage_limit_reached"}}`,
		Unavailable:    false,
		NextRetryAfter: nextRetry,
		LastError:      &coreauth.Error{Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRecover,
			Window:        "5h",
			WindowMinutes: 300,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.4-mini": makeModelState(),
			"gpt-5.5":      makeModelState(),
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	restrictions, ok := entry["restrictions"].([]map[string]any)
	if !ok {
		t.Fatalf("restrictions type = %T, want []map[string]any", entry["restrictions"])
	}
	if len(restrictions) != 1 {
		t.Fatalf("restrictions length = %d, want 1", len(restrictions))
	}
	got := restrictions[0]
	if got["scope"] != "auth" || got["http_status"] != http.StatusTooManyRequests {
		t.Fatalf("restriction = %#v, want auth 429", got)
	}
	if got["quota_window"] != "5h" || got["quota_window_minutes"] != 300 {
		t.Fatalf("quota window = %#v/%#v, want 5h/300", got["quota_window"], got["quota_window_minutes"])
	}
	if _, hasModel := got["model"]; hasModel {
		t.Fatalf("restriction model = %#v, want no model field", got["model"])
	}
}

func TestBuildAuthFileEntryIncludesSubscriptionExpiration(t *testing.T) {
	expiresAt := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Minute)
	auth := &coreauth.Auth{
		ID:       "codex-subscription",
		FileName: "codex-subscription.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-subscription.json",
		},
		Metadata: map[string]any{
			"subscription_expires_at": expiresAt.Format(time.RFC3339),
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	if got, _ := entry["subscription_expires_at"].(string); got != expiresAt.Format(time.RFC3339) {
		t.Fatalf("subscription_expires_at = %q, want %q", got, expiresAt.Format(time.RFC3339))
	}
	if got, ok := entry["subscription_expires_at_ms"].(int64); !ok || got != expiresAt.UnixMilli() {
		t.Fatalf("subscription_expires_at_ms = %#v, want %d", entry["subscription_expires_at_ms"], expiresAt.UnixMilli())
	}
	if got, ok := entry["subscription_remaining_minutes"].(int64); !ok || got < 89 || got > 90 {
		t.Fatalf("subscription_remaining_minutes = %#v, want around 90", entry["subscription_remaining_minutes"])
	}
	if expired, _ := entry["subscription_expired"].(bool); expired {
		t.Fatal("subscription_expired = true, want false")
	}
}

func TestBuildAuthFileEntryIncludesSubscriptionStartAndPeriod(t *testing.T) {
	startedAt := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "codex-subscription-start",
		FileName: "codex-subscription-start.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "codex-subscription-start.json",
		},
		Metadata: map[string]any{
			"subscription_started_at": startedAt.Format(time.RFC3339),
			"subscription_period":     "yearly",
		},
	}

	entry := managementauthfiles.BuildEntry(auth, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("expected auth file entry")
	}
	if got, _ := entry["subscription_started_at"].(string); got != startedAt.Format(time.RFC3339) {
		t.Fatalf("subscription_started_at = %q, want %q", got, startedAt.Format(time.RFC3339))
	}
	if got, ok := entry["subscription_started_at_ms"].(int64); !ok || got != startedAt.UnixMilli() {
		t.Fatalf("subscription_started_at_ms = %#v, want %d", entry["subscription_started_at_ms"], startedAt.UnixMilli())
	}
	if got, _ := entry["subscription_period"].(string); got != "yearly" {
		t.Fatalf("subscription_period = %q, want yearly", got)
	}
	wantExpiresAt := startedAt.AddDate(1, 0, 0)
	if got, _ := entry["subscription_expires_at"].(string); got != wantExpiresAt.Format(time.RFC3339) {
		t.Fatalf("subscription_expires_at = %q, want %q", got, wantExpiresAt.Format(time.RFC3339))
	}
	if got, ok := entry["subscription_expires_at_ms"].(int64); !ok || got != wantExpiresAt.UnixMilli() {
		t.Fatalf("subscription_expires_at_ms = %#v, want %d", entry["subscription_expires_at_ms"], wantExpiresAt.UnixMilli())
	}
	if _, ok := entry["subscription_remaining_days"].(int64); !ok {
		t.Fatalf("subscription_remaining_days = %#v, want int64", entry["subscription_remaining_days"])
	}
}

func TestPatchAuthFileFieldsUpdatesSubscriptionExpiration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-subscription",
		FileName: "oauth-subscription.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "subscriber@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":                    "oauth-subscription.json",
		"subscription_expires_at": "2027-01-02T03:04:00Z",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-subscription")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if got, _ := updated.Metadata["subscription_expires_at"].(string); got != "2027-01-02T03:04:00Z" {
		t.Fatalf("subscription_expires_at = %q, want %q", got, "2027-01-02T03:04:00Z")
	}
}

func TestPatchAuthFileFieldsUpdatesSubscriptionStartAndPeriod(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-subscription-start",
		FileName: "oauth-subscription-start.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":                   "subscriber@example.com",
			"subscription_expires_at": "2099-01-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":                    "oauth-subscription-start.json",
		"subscription_started_at": "2027-01-02T03:04:00Z",
		"subscription_period":     "yearly",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-subscription-start")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if got, _ := updated.Metadata["subscription_started_at"].(string); got != "2027-01-02T03:04:00Z" {
		t.Fatalf("subscription_started_at = %q, want %q", got, "2027-01-02T03:04:00Z")
	}
	if got, _ := updated.Metadata["subscription_period"].(string); got != "yearly" {
		t.Fatalf("subscription_period = %q, want yearly", got)
	}
	if _, exists := updated.Metadata["subscription_expires_at"]; exists {
		t.Fatalf("subscription_expires_at should be cleared, got %v", updated.Metadata["subscription_expires_at"])
	}
}

func TestPatchAuthFileFieldsClearsSubscriptionExpiration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-subscription-clear",
		FileName: "oauth-subscription-clear.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":                   "subscriber@example.com",
			"subscription_expires_at": "2027-01-02T03:04:00Z",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":                    "oauth-subscription-clear.json",
		"subscription_expires_at": "",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-subscription-clear")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if _, exists := updated.Metadata["subscription_expires_at"]; exists {
		t.Fatalf("subscription_expires_at should be cleared, got %v", updated.Metadata["subscription_expires_at"])
	}
}

func TestPatchAuthFileFieldsRenamesRoutingChannelReferences(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	if err := usage.ReplaceAllAPIKeyPermissionProfiles([]usage.APIKeyPermissionProfileRow{
		{
			ID:              "standard",
			Name:            "Standard",
			AllowedChannels: []string{"Team Old", "Other Channel"},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllAPIKeyPermissionProfiles: %v", err)
	}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-routing",
		FileName: "oauth-auth-routing.json",
		Provider: "claude",
		Label:    "Team Old",
		Metadata: map[string]any{
			"email": "team-old@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{
			Routing: config.RoutingConfig{
				ChannelGroups: []config.RoutingChannelGroup{
					{
						Name: "team-alpha",
						Match: config.ChannelGroupMatch{
							Channels: []string{"Team Old", "Other Channel"},
						},
						ChannelPriorities: map[string]int{
							"Team Old":      80,
							"Other Channel": 10,
						},
					},
				},
			},
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"team old": {{Name: "claude-sonnet", Alias: "sonnet"}},
			},
			SDKConfig: config.SDKConfig{
				APIKeyEntries: []config.APIKeyEntry{
					{Key: "sk-test", Name: "test", AllowedChannels: []string{"Team Old"}},
				},
			},
		},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "oauth-auth-routing.json",
		"label": "Team New",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	group := h.cfg.Routing.ChannelGroups[0]
	if !containsString(group.Match.Channels, "Team New") {
		t.Fatalf("match.channels = %v, want renamed channel", group.Match.Channels)
	}
	if containsString(group.Match.Channels, "Team Old") {
		t.Fatalf("match.channels = %v, should not keep old channel", group.Match.Channels)
	}
	if got := group.ChannelPriorities["Team New"]; got != 80 {
		t.Fatalf("channel-priorities[Team New] = %d, want 80; map=%v", got, group.ChannelPriorities)
	}
	if _, exists := group.ChannelPriorities["Team Old"]; exists {
		t.Fatalf("channel-priorities = %v, should not keep old key", group.ChannelPriorities)
	}
	if _, exists := h.cfg.OAuthModelAlias["team old"]; exists {
		t.Fatalf("oauth-model-alias still has old channel: %v", h.cfg.OAuthModelAlias)
	}
	if _, exists := h.cfg.OAuthModelAlias["team new"]; !exists {
		t.Fatalf("oauth-model-alias missing new channel: %v", h.cfg.OAuthModelAlias)
	}
	if !containsString(h.cfg.APIKeyEntries[0].AllowedChannels, "Team New") {
		t.Fatalf("allowed-channels = %v, want renamed channel", h.cfg.APIKeyEntries[0].AllowedChannels)
	}
	profiles := usage.ListAPIKeyPermissionProfiles()
	if len(profiles) != 1 {
		t.Fatalf("profiles len = %d, want 1", len(profiles))
	}
	if !containsString(profiles[0].AllowedChannels, "Team New") {
		t.Fatalf("profile allowed-channels = %v, want renamed channel", profiles[0].AllowedChannels)
	}
	if containsString(profiles[0].AllowedChannels, "Team Old") {
		t.Fatalf("profile allowed-channels = %v, should not keep old channel", profiles[0].AllowedChannels)
	}
}

func TestPatchAuthFileFieldsRejectsDuplicateOAuthChannelLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-2",
		FileName: "oauth-auth-2.json",
		Provider: "gemini",
		Metadata: map[string]any{
			"email": "oauth@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{Name: "Shared Channel", APIKey: "claude-api-key"},
			},
		},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "oauth-auth-2.json",
		"label": "shared channel",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-2")
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if updated.Label != "" {
		t.Fatalf("label = %q, want empty", updated.Label)
	}
	if _, exists := updated.Metadata["label"]; exists {
		t.Fatalf("unexpected metadata label after rejected update: %v", updated.Metadata["label"])
	}
}

func TestPatchAuthFileFieldsAllowsRenameWhenUnrelatedOAuthAliasesConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "oauth-alias-a",
			FileName: "oauth-alias-a.json",
			Provider: "codex",
			Label:    "Team A",
			Metadata: map[string]any{"email": "shared@example.com"},
		},
		{
			ID:       "oauth-alias-b",
			FileName: "oauth-alias-b.json",
			Provider: "codex",
			Label:    "Team B",
			Metadata: map[string]any{"email": "shared@example.com"},
		},
		{
			ID:       "oauth-auth-rename",
			FileName: "oauth-auth-rename.json",
			Provider: "claude",
			Label:    "Original Channel",
			Metadata: map[string]any{"email": "rename@example.com"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "oauth-auth-rename.json",
		"label": "Renamed Channel",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-rename")
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if updated.Label != "Renamed Channel" {
		t.Fatalf("label = %q, want %q", updated.Label, "Renamed Channel")
	}
}

func TestPatchAuthFileFieldsReturnsErrorWhenPersistenceFails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-3",
		FileName: "oauth-auth-3.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "persist@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.SetStore(&failingAuthStore{
		items: map[string]*coreauth.Auth{
			"oauth-auth-3": {
				ID:        "oauth-auth-3",
				FileName:  "oauth-auth-3.json",
				Provider:  "codex",
				Metadata:  map[string]any{"email": "persist@example.com"},
				UpdatedAt: time.Now(),
			},
		},
	})

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body, err := json.Marshal(map[string]any{
		"name":  "oauth-auth-3.json",
		"label": "Broken Persist",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/fields", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("oauth-auth-3")
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if updated.ChannelName() == "Broken Persist" {
		t.Fatalf("expected in-memory auth rollback on persist failure, got channel=%q", updated.ChannelName())
	}
}
