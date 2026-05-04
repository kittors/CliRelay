package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

	entry := (&Handler{}).buildAuthFileEntry(auth)
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

	entry := (&Handler{}).buildAuthFileEntry(auth)
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

func TestClaudeOAuthMetadataFromTokenStorageIncludesRuntimeTokens(t *testing.T) {
	expired := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	lastRefresh := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)

	meta := claudeOAuthMetadataFromTokenStorage(&claude.ClaudeTokenStorage{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Email:        "claude@example.com",
		Expire:       expired,
		LastRefresh:  lastRefresh,
	})

	for key, want := range map[string]string{
		"type":          "claude",
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"email":         "claude@example.com",
		"expired":       expired,
		"last_refresh":  lastRefresh,
	} {
		if got, _ := meta[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
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
