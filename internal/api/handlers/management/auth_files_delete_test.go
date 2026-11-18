package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestDeleteAuthFileRemovesDeletedChannelReferences(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	authDir := t.TempDir()
	authAPath := filepath.Join(authDir, "kimi-a.json")
	authBPath := filepath.Join(authDir, "kimi-b.json")
	if err := os.WriteFile(authAPath, []byte(`{"type":"kimi","access_token":"at-a","refresh_token":"rt-a","label":"kimi-A"}`), 0o600); err != nil {
		t.Fatalf("write auth A: %v", err)
	}
	if err := os.WriteFile(authBPath, []byte(`{"type":"kimi","access_token":"at-b","refresh_token":"rt-b","label":"kimi-B"}`), 0o600); err != nil {
		t.Fatalf("write auth B: %v", err)
	}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "kimi-a.json",
			FileName: "kimi-a.json",
			Provider: "kimi",
			Label:    "kimi-A",
			Metadata: map[string]any{
				"type":          "kimi",
				"access_token":  "at-a",
				"refresh_token": "rt-a",
				"label":         "kimi-A",
			},
			Attributes: map[string]string{"path": authAPath, "source": authAPath},
		},
		{
			ID:       "kimi-b.json",
			FileName: "kimi-b.json",
			Provider: "kimi",
			Label:    "kimi-B",
			Metadata: map[string]any{
				"type":          "kimi",
				"access_token":  "at-b",
				"refresh_token": "rt-b",
				"label":         "kimi-B",
			},
			Attributes: map[string]string{"path": authBPath, "source": authBPath},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.FileName, err)
		}
	}

	if err := usage.ReplaceAllAPIKeyPermissionProfiles([]usage.APIKeyPermissionProfileRow{
		{
			ID:              "hermes",
			Name:            "Hermes",
			AllowedChannels: []string{"kimi-A", "kimi-B"},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllAPIKeyPermissionProfiles: %v", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		Key:                 "sk-bound-profile",
		Name:                "Bound Profile",
		PermissionProfileID: "hermes",
		AllowedChannels:     []string{"kimi-A", "kimi-B"},
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
			Routing: config.RoutingConfig{
				ChannelGroups: []config.RoutingChannelGroup{
					{
						Name: "kimi-mix",
						Match: config.ChannelGroupMatch{
							Channels: []string{"kimi-A", "kimi-B"},
						},
						ChannelPriorities: map[string]int{
							"kimi-A": 90,
							"kimi-B": 20,
						},
					},
				},
			},
			OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"kimi-a": {{Name: "kimi-k2", Alias: "kimi-a-k2"}},
				"kimi-b": {{Name: "kimi-k2", Alias: "kimi-b-k2"}},
			},
			SDKConfig: config.SDKConfig{
				APIKeyEntries: []config.APIKeyEntry{
					{Key: "sk-config-entry", Name: "Config Entry", AllowedChannels: []string{"kimi-A", "kimi-B"}},
				},
			},
		},
		authManager: manager,
		tokenStore:  store,
	}
	moderationStore := contentmoderation.NewStore(usage.RuntimeDB())
	moderationProfile, err := contentmoderation.NewProfile(identity.SystemTenantID, "profile-auth-cleanup", contentmoderation.CreateProfileInput{Name: "auth cleanup"}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = moderationStore.CreateProfile(context.Background(), moderationProfile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := moderationProfile.ID
	if err = moderationStore.PatchBindings(context.Background(), identity.SystemTenantID, false, []contentmoderation.BindingOperation{{
		ChannelType: contentmoderation.ChannelTypeAuthFile,
		ChannelID:   "kimi-a.json",
		ProfileID:   &profileID,
	}}); err != nil {
		t.Fatalf("PatchBindings: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/auth-files?name=kimi-a.json", nil)

	h.DeleteAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if _, ok := manager.GetByID("kimi-a.json"); ok {
		t.Fatal("expected deleted auth to be removed from manager")
	}
	if _, ok := manager.GetByID("kimi-b.json"); !ok {
		t.Fatal("expected remaining auth to stay in manager")
	}
	if _, _, err = moderationStore.ResolveProfile(context.Background(), identity.SystemTenantID, "kimi-a.json", "", ""); !errors.Is(err, contentmoderation.ErrNotFound) {
		t.Fatalf("ResolveProfile error=%v, want ErrNotFound", err)
	}

	profiles := usage.ListAPIKeyPermissionProfiles()
	if len(profiles) != 1 {
		t.Fatalf("profiles len = %d, want 1", len(profiles))
	}
	if containsString(profiles[0].AllowedChannels, "kimi-A") {
		t.Fatalf("profile allowed-channels = %v, should not keep deleted channel", profiles[0].AllowedChannels)
	}
	if !containsString(profiles[0].AllowedChannels, "kimi-B") {
		t.Fatalf("profile allowed-channels = %v, should keep remaining channel", profiles[0].AllowedChannels)
	}

	row := usage.GetAPIKey("sk-bound-profile")
	if row == nil {
		t.Fatal("expected API key row")
	}
	if containsString(row.AllowedChannels, "kimi-A") {
		t.Fatalf("api key allowed-channels = %v, should not keep deleted channel", row.AllowedChannels)
	}
	if !containsString(row.AllowedChannels, "kimi-B") {
		t.Fatalf("api key allowed-channels = %v, should keep remaining channel", row.AllowedChannels)
	}

	group := h.cfg.Routing.ChannelGroups[0]
	if containsString(group.Match.Channels, "kimi-A") {
		t.Fatalf("routing match channels = %v, should not keep deleted channel", group.Match.Channels)
	}
	if _, exists := group.ChannelPriorities["kimi-A"]; exists {
		t.Fatalf("routing channel priorities = %v, should not keep deleted channel", group.ChannelPriorities)
	}
	if containsString(h.cfg.APIKeyEntries[0].AllowedChannels, "kimi-A") {
		t.Fatalf("config api key allowed-channels = %v, should not keep deleted channel", h.cfg.APIKeyEntries[0].AllowedChannels)
	}
	if _, exists := h.cfg.OAuthModelAlias["kimi-a"]; exists {
		t.Fatalf("oauth model aliases = %v, should not keep deleted channel", h.cfg.OAuthModelAlias)
	}
	if _, exists := h.cfg.OAuthModelAlias["kimi-b"]; !exists {
		t.Fatalf("oauth model aliases = %v, should keep remaining channel", h.cfg.OAuthModelAlias)
	}
}
