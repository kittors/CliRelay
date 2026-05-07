package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func testRoutingConfig() *config.Config {
	return &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeyEntries: []config.APIKeyEntry{
				{
					Key:                  "sk-team-a",
					Name:                 "Team A",
					AllowedChannelGroups: []string{"team-a"},
					AllowedChannels:      []string{"Team A Codex"},
				},
			},
		},
		CodexKey: []config.CodexKey{
			{APIKey: "sk-pro", Name: "Pro Codex", Prefix: "pro"},
			{APIKey: "sk-team-a", Name: "Team A Codex", Prefix: "pro"},
			{APIKey: "sk-default", Name: "Default Codex"},
		},
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:        "pro",
					Description: "Pro channels",
					Priority:    100,
					Strategy:    "round-robin",
					Match: config.ChannelGroupMatch{
						Prefixes: []string{"pro"},
					},
				},
				{
					Name:        "team-a",
					Description: "Team A channels",
					Priority:    50,
					Strategy:    "fill-first",
					Match: config.ChannelGroupMatch{
						Channels: []string{"Team A Codex"},
					},
				},
			},
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/pro", Group: "pro", StripPrefix: true},
				{Path: "/team-a", Group: "team-a", StripPrefix: true},
			},
		},
	}
}

func TestBuildChannelGroupItemsIncludesExplicitImplicitAndRoutes(t *testing.T) {
	items := buildChannelGroupItems(testRoutingConfig(), nil)
	if len(items) < 3 {
		t.Fatalf("expected at least 3 groups, got %d", len(items))
	}

	byName := make(map[string]channelGroupItem, len(items))
	for _, item := range items {
		byName[item.Name] = item
	}

	pro, ok := byName["pro"]
	if !ok {
		t.Fatal("expected pro group")
	}
	if pro.Implicit {
		t.Fatal("expected pro group to be explicit")
	}
	if pro.Priority != 100 {
		t.Fatalf("pro priority = %d, want 100", pro.Priority)
	}
	if pro.Strategy != "round-robin" {
		t.Fatalf("pro strategy = %q, want round-robin", pro.Strategy)
	}
	if strings.Join(pro.PathRoutes, ",") != "/pro" {
		t.Fatalf("pro path-routes = %v, want [/pro]", pro.PathRoutes)
	}
	if !containsString(pro.Channels, "Pro Codex") || !containsString(pro.Channels, "Team A Codex") {
		t.Fatalf("pro channels = %v, want both prefixed channels", pro.Channels)
	}

	teamA, ok := byName["team-a"]
	if !ok {
		t.Fatal("expected team-a group")
	}
	if teamA.Implicit {
		t.Fatal("expected team-a group to be explicit")
	}
	if teamA.Strategy != "fill-first" {
		t.Fatalf("team-a strategy = %q, want fill-first", teamA.Strategy)
	}
	if strings.Join(teamA.PathRoutes, ",") != "/team-a" {
		t.Fatalf("team-a path-routes = %v, want [/team-a]", teamA.PathRoutes)
	}
	if !containsString(teamA.Channels, "Team A Codex") {
		t.Fatalf("team-a channels = %v, want Team A Codex", teamA.Channels)
	}

	defaultGroup, ok := byName["default"]
	if !ok {
		t.Fatal("expected default group")
	}
	if !defaultGroup.Implicit {
		t.Fatal("expected default group to be implicit")
	}
	if !containsString(defaultGroup.Channels, "Default Codex") {
		t.Fatalf("default channels = %v, want Default Codex", defaultGroup.Channels)
	}
}

func TestValidateRoutingAndAPIKeyRestrictions(t *testing.T) {
	t.Run("accepts valid config", func(t *testing.T) {
		if err := validateRoutingAndAPIKeyRestrictions(testRoutingConfig(), nil); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
	})

	testCases := []struct {
		name        string
		mutate      func(*config.Config)
		wantMessage string
	}{
		{
			name: "duplicate group names are rejected",
			mutate: func(cfg *config.Config) {
				cfg.Routing.ChannelGroups = append(cfg.Routing.ChannelGroups, config.RoutingChannelGroup{
					Name:  "PRO",
					Match: config.ChannelGroupMatch{Prefixes: []string{"pro"}},
				})
			},
			wantMessage: `duplicate channel group "PRO"`,
		},
		{
			name: "empty group is allowed",
			mutate: func(cfg *config.Config) {
				cfg.Routing.ChannelGroups = append(cfg.Routing.ChannelGroups, config.RoutingChannelGroup{
					Name:  "ghost",
					Match: config.ChannelGroupMatch{Prefixes: []string{"ghost"}},
				})
			},
			wantMessage: "",
		},
		{
			name: "duplicate path routes are rejected",
			mutate: func(cfg *config.Config) {
				cfg.Routing.PathRoutes = append(cfg.Routing.PathRoutes, config.RoutingPathRoute{
					Path:  "/pro",
					Group: "team-a",
				})
			},
			wantMessage: `duplicate path route "/pro"`,
		},
		{
			name: "reserved path routes are rejected",
			mutate: func(cfg *config.Config) {
				cfg.Routing.PathRoutes = append(cfg.Routing.PathRoutes, config.RoutingPathRoute{
					Path:  "/v1",
					Group: "pro",
				})
			},
			wantMessage: `path route "/v1" conflicts with reserved internal path`,
		},
		{
			name: "path route group must exist",
			mutate: func(cfg *config.Config) {
				cfg.Routing.PathRoutes = append(cfg.Routing.PathRoutes, config.RoutingPathRoute{
					Path:  "/free",
					Group: "free",
				})
			},
			wantMessage: `path route "/free" references unknown channel group "free"`,
		},
		{
			name: "api key groups must exist",
			mutate: func(cfg *config.Config) {
				cfg.APIKeyEntries[0].AllowedChannelGroups = []string{"missing"}
			},
			wantMessage: `api-key "Team A" references unknown channel group "missing"`,
		},
		{
			name: "api key channel and group restrictions must intersect",
			mutate: func(cfg *config.Config) {
				cfg.APIKeyEntries[0].AllowedChannels = []string{"Pro Codex"}
			},
			wantMessage: `api-key "Team A" allowed-channels do not belong to allowed-channel-groups`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := testRoutingConfig()
			tc.mutate(cfg)

			err := validateRoutingAndAPIKeyRestrictions(cfg, nil)
			if tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("expected no validation error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.wantMessage)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("validation error = %q, want substring %q", err.Error(), tc.wantMessage)
			}
		})
	}
}

func TestGetChannelGroupsReturnsGroupMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/channel-groups", nil)

	h := NewHandler(testRoutingConfig(), "", nil)
	h.GetChannelGroups(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Items []channelGroupItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Items) < 3 {
		t.Fatalf("expected at least 3 group items, got %d", len(body.Items))
	}
}

func TestGetChannelGroupsReturnsChannelDetailsWithTags(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-tags",
		FileName: "oauth-auth-tags.json",
		Provider: "codex",
		Label:    "A_GptPro",
		Prefix:   "team-a",
		Metadata: map[string]any{
			"email":               "tags@example.com",
			"plan_type":           "pro",
			"custom_tags":         []string{"vip"},
			"hidden_default_tags": []string{"pro"},
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/channel-groups", nil)

	h := &Handler{
		cfg:         testRoutingConfig(),
		authManager: manager,
	}
	h.GetChannelGroups(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Items []struct {
			Name           string `json:"name"`
			ChannelDetails []struct {
				Name        string   `json:"name"`
				DefaultTags []string `json:"default_tags"`
				DisplayTags []string `json:"display_tags"`
			} `json:"channel-details"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var matched *struct {
		Name           string `json:"name"`
		ChannelDetails []struct {
			Name        string   `json:"name"`
			DefaultTags []string `json:"default_tags"`
			DisplayTags []string `json:"display_tags"`
		} `json:"channel-details"`
	}
	for i := range body.Items {
		if body.Items[i].Name == "team-a" {
			matched = &body.Items[i]
			break
		}
	}
	if matched == nil {
		t.Fatal("expected team-a group")
	}
	if len(matched.ChannelDetails) == 0 {
		t.Fatal("expected channel details for team-a group")
	}
	if matched.ChannelDetails[0].Name != "A_GptPro" {
		t.Fatalf("channel detail name = %q, want A_GptPro", matched.ChannelDetails[0].Name)
	}
	if len(matched.ChannelDetails[0].DefaultTags) != 2 ||
		matched.ChannelDetails[0].DefaultTags[0] != "codex" ||
		matched.ChannelDetails[0].DefaultTags[1] != "pro" {
		t.Fatalf("default_tags = %#v, want [codex pro]", matched.ChannelDetails[0].DefaultTags)
	}
	if len(matched.ChannelDetails[0].DisplayTags) != 2 ||
		matched.ChannelDetails[0].DisplayTags[0] != "codex" ||
		matched.ChannelDetails[0].DisplayTags[1] != "vip" {
		t.Fatalf("display_tags = %#v, want [codex vip]", matched.ChannelDetails[0].DisplayTags)
	}
}

func TestBuildChannelGroupItemsCanonicalizesRenamedOAuthChannel(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name: "team-alpha",
					Match: config.ChannelGroupMatch{
						Channels: []string{"gcqcdaihyrte@outlook.com"},
					},
				},
			},
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/team-alpha", Group: "team-alpha", StripPrefix: true},
			},
		},
	}
	auths := []*coreauth.Auth{
		{
			ID:       "oauth-1",
			Label:    "chatgpt-pro1",
			Prefix:   "team-alpha",
			Provider: "claude",
			Metadata: map[string]any{
				"email": "gcqcdaihyrte@outlook.com",
			},
		},
	}

	items := buildChannelGroupItems(cfg, auths)
	if len(items) != 1 {
		t.Fatalf("expected 1 group, got %d", len(items))
	}
	if !containsString(items[0].Channels, "chatgpt-pro1") {
		t.Fatalf("group channels = %v, want canonical renamed channel", items[0].Channels)
	}
	if containsString(items[0].Channels, "gcqcdaihyrte@outlook.com") {
		t.Fatalf("group channels = %v, should not contain legacy email alias", items[0].Channels)
	}
}

func TestBuildChannelGroupItemsSkipsDisabledAuthChannels(t *testing.T) {
	auths := []*coreauth.Auth{
		{
			ID:       "active-auth",
			Label:    "Active Channel",
			Prefix:   "team-a",
			Provider: "codex",
		},
		{
			ID:            "deleted-auth",
			Label:         "Deleted Channel",
			Prefix:        "team-b",
			Provider:      "claude",
			Disabled:      true,
			Status:        coreauth.StatusDisabled,
			StatusMessage: "removed via management api",
		},
	}

	items := buildChannelGroupItems(&config.Config{}, auths)
	byName := make(map[string]channelGroupItem, len(items))
	for _, item := range items {
		byName[item.Name] = item
	}

	teamA, ok := byName["team-a"]
	if !ok {
		t.Fatal("expected active team-a group")
	}
	if !containsString(teamA.Channels, "Active Channel") {
		t.Fatalf("team-a channels = %v, want Active Channel", teamA.Channels)
	}
	if containsString(teamA.Channels, "Deleted Channel") {
		t.Fatalf("team-a channels = %v, should not contain deleted channel", teamA.Channels)
	}
	if _, exists := byName["team-b"]; exists {
		t.Fatalf("unexpected lingering team-b group from deleted auth: %v", byName["team-b"])
	}
}

func TestBuildChannelGroupItemsDoesNotSurfaceDeletedConfiguredChannels(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name: "chatgpt-pro",
					Match: config.ChannelGroupMatch{
						Channels: []string{"chatgpt-pro1"},
					},
				},
			},
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/openai/pro", Group: "chatgpt-pro", StripPrefix: true},
			},
		},
	}

	items := buildChannelGroupItems(cfg, nil)
	if len(items) != 1 {
		t.Fatalf("expected 1 group, got %d", len(items))
	}
	if items[0].Name != "chatgpt-pro" {
		t.Fatalf("group name = %q, want chatgpt-pro", items[0].Name)
	}
	if len(items[0].Channels) != 0 {
		t.Fatalf("group channels = %v, want no active channels for deleted references", items[0].Channels)
	}
	if !containsString(items[0].PathRoutes, "/openai/pro") {
		t.Fatalf("path-routes = %v, want /openai/pro", items[0].PathRoutes)
	}
}

func TestCanonicalizeRoutingConfigChannelsRenamedOAuthChannel(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name: "team-alpha",
					Match: config.ChannelGroupMatch{
						Channels: []string{"gcqcdaihyrte@outlook.com"},
					},
					ChannelPriorities: map[string]int{
						"gcqcdaihyrte@outlook.com": 100,
					},
				},
			},
		},
	}
	auths := []*coreauth.Auth{
		{
			ID:       "oauth-1",
			Label:    "chatgpt-pro1",
			Provider: "claude",
			Metadata: map[string]any{
				"email": "gcqcdaihyrte@outlook.com",
			},
		},
	}

	known, err := collectKnownChannels(cfg, auths, "")
	if err != nil {
		t.Fatalf("collectKnownChannels() error = %v", err)
	}
	got := canonicalizeRoutingConfigChannels(currentRoutingConfig(cfg), known)
	if !containsString(got.ChannelGroups[0].Match.Channels, "chatgpt-pro1") {
		t.Fatalf("match.channels = %v, want canonical renamed channel", got.ChannelGroups[0].Match.Channels)
	}
	if _, exists := got.ChannelGroups[0].ChannelPriorities["chatgpt-pro1"]; !exists {
		t.Fatalf("channel-priorities = %v, want canonical renamed key", got.ChannelGroups[0].ChannelPriorities)
	}
	if _, exists := got.ChannelGroups[0].ChannelPriorities["gcqcdaihyrte@outlook.com"]; exists {
		t.Fatalf("channel-priorities = %v, should not contain legacy email alias", got.ChannelGroups[0].ChannelPriorities)
	}
}

func TestPutConfigYAMLRejectsInvalidRoutingRestrictions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := strings.NewReader(`
codex-api-key:
  - api-key: "sk-pro"
    name: "Pro Codex"
    prefix: "pro"
routing:
  include-default-group: true
  channel-groups:
    - name: "pro"
      match:
        prefixes: ["pro"]
  path-routes:
    - path: "/v1"
      group: "pro"
`)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", body)

	h := NewHandler(&config.Config{}, configPath, nil)
	h.PutConfigYAML(c)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reserved internal path") {
		t.Fatalf("expected reserved path validation error, got %s", rec.Body.String())
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
