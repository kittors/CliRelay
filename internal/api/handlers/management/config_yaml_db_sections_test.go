package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v8/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
)

// A raw config.yaml save that changes a section the database already holds
// (routing, proxy-pool, the API key sections) is rejected before anything is
// stored or written. The reload after the save used to drop such a section
// from config.yaml and apply the stored value while the save answered 200;
// API keys even served as keys of this node alone.
func TestPutConfigYAMLRejectsChangedDatabaseOwnedSections(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T)
		section string
		yaml    string
		home    string
		stored  func(t *testing.T)
	}{
		{
			name:    "routing",
			seed:    seedStoredRouting,
			section: "routing",
			// Typed into an editor that never shows the stored routing: storing
			// it would drop the stored channel group and path route.
			yaml:   "routing:\n  strategy: round-robin\n",
			home:   "Channel Groups page",
			stored: assertStoredRoutingUnchanged,
		},
		{
			name:    "proxy-pool",
			seed:    seedStoredProxyPool,
			section: "proxy-pool",
			// Storing it would delete both stored proxies.
			yaml:   "proxy-pool:\n  - id: new-proxy\n    url: http://new-proxy.example:8080\n    enabled: true\n",
			home:   "Outbound Proxies page",
			stored: assertStoredProxyPoolUnchanged,
		},
		{
			name:    "api-keys",
			seed:    seedStoredAPIKey,
			section: "api-keys",
			yaml:    "api-keys:\n  - " + yamlSaveShadowKey + "\n",
			home:    "API Keys page",
			stored:  assertShadowKeyNotStored,
		},
		{
			name:    "api-key-entries",
			seed:    seedStoredAPIKey,
			section: "api-key-entries",
			yaml:    "api-key-entries:\n  - key: " + yamlSaveShadowKey + "\n    name: shadow\n",
			home:    "API Keys page",
			stored:  assertShadowKeyNotStored,
		},
		{
			name:    "api-key-permission-profiles",
			seed:    seedStoredPermissionProfile,
			section: "api-key-permission-profiles",
			yaml:    "api-key-permission-profiles:\n  - id: yaml-profile\n    name: From YAML\n",
			home:    "Portal Account Permissions page",
			stored:  assertStoredPermissionProfilesUnchanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := newDatabaseOwnedSectionsNode(t, tc.seed)
			liveRouting, livePool := node.h.cfg.Routing, node.h.cfg.ProxyPool
			// A runtime setting in the same save must not be stored either.
			body := "# operator notes\nport: 8319\nproxy-url: " + yamlSaveNewProxy + "\n" + tc.yaml

			rec := putConfigYAML(node.h, []byte(body))

			rejection := decodeDatabaseOwnedRejection(t, rec)
			if len(rejection.Details.Sections) != 1 || rejection.Details.Sections[0] != tc.section {
				t.Fatalf("rejected sections = %v, want [%s]", rejection.Details.Sections, tc.section)
			}
			if !strings.Contains(rejection.Message, tc.section) || !strings.Contains(rejection.Message, tc.home) {
				t.Fatalf("message %q should name %s and %s", rejection.Message, tc.section, tc.home)
			}
			tc.stored(t)
			assertStoredProxyURL(t, yamlSaveOldProxy)
			assertFileContent(t, node.configPath, node.original)
			if node.hookCalls != 0 || node.h.cfg.ProxyURL != yamlSaveOldProxy || node.h.cfg.Port != 8318 {
				t.Fatalf("a rejected save changed the live config: hook calls=%d proxy-url=%q port=%d", node.hookCalls, node.h.cfg.ProxyURL, node.h.cfg.Port)
			}
			if !config.SameRoutingConfig(node.h.cfg.Routing, liveRouting) || !config.SameProxyPool(node.h.cfg.ProxyPool, livePool) {
				t.Fatal("a rejected save changed the live routing or proxy pool")
			}
			if authenticatesOnNode(yamlSaveShadowKey) {
				t.Fatal("a key from a rejected save authenticates")
			}
		})
	}
}

// The panel's visual editor sends back a routing section it found in the
// file, and an operator may paste the stored value; neither changes anything,
// so the save goes through and the reload drops the copy from config.yaml.
func TestPutConfigYAMLAcceptsCopiesOfStoredSections(t *testing.T) {
	node := newDatabaseOwnedSectionsNode(t, func(t *testing.T) {
		seedStoredRouting(t)
		seedStoredProxyPool(t)
	})
	// The same values as stored, spelled differently: a strategy alias, group
	// names in another case, a path with slashes, include-default-group left
	// to its default, proxies in another order with an upper-case ID and a
	// padded URL.
	body := `port: 8319
routing:
  strategy: ff
  channel-groups:
    - name: Team-A
      match:
        prefixes:
          - Team-A
  path-routes:
    - path: /team-a/
      group: Team-A
proxy-pool:
  - id: SG-1
    name: SG 1
    url: " socks5://sg-1.example:1080 "
    enabled: false
  - id: hk-1
    name: HK 1
    url: http://hk-1.example:8080
    enabled: true
`

	rec := putConfigYAML(node.h, []byte(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
	}
	assertStoredRoutingUnchanged(t)
	assertStoredProxyPoolUnchanged(t)
	data := readConfigFile(t, node.configPath)
	if strings.Contains(data, "routing:") || strings.Contains(data, "proxy-pool:") || !strings.Contains(data, "port: 8319") {
		t.Fatalf("config.yaml should keep port and lose the stored sections:\n%s", data)
	}
	if node.h.cfg.Port != 8319 || !config.SameRoutingConfig(node.h.cfg.Routing, storedRouting()) || !config.SameProxyPool(node.h.cfg.ProxyPool, storedProxyPool()) {
		t.Fatalf("live config after the save: port=%d routing=%+v proxy-pool=%+v", node.h.cfg.Port, node.h.cfg.Routing, node.h.cfg.ProxyPool)
	}
}

// A proxy pool the database does not hold yet is imported by the reload after
// the save, as before. (Routing has no such state past the first start, which
// stores the routing every config.yaml implies.)
func TestPutConfigYAMLStillImportsAProxyPoolTheDatabaseDoesNotHold(t *testing.T) {
	node := newDatabaseOwnedSectionsNode(t, nil)
	body := "port: 8319\nproxy-pool:\n  - id: hk-1\n    url: http://hk-1.example:8080\n    enabled: true\n"

	rec := putConfigYAML(node.h, []byte(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
	}
	if stored := usage.ListProxyPool(); len(stored) != 1 || stored[0].ID != "hk-1" {
		t.Fatalf("stored proxy pool = %+v, want the imported hk-1", stored)
	}
	if len(node.h.cfg.ProxyPool) != 1 || node.h.cfg.ProxyPool[0].ID != "hk-1" {
		t.Fatalf("live proxy pool = %+v, want the imported hk-1", node.h.cfg.ProxyPool)
	}
	if data := readConfigFile(t, node.configPath); strings.Contains(data, "proxy-pool:") || !strings.Contains(data, "port: 8319") {
		t.Fatalf("the imported proxy pool should be dropped from config.yaml:\n%s", data)
	}
}

// API keys written into config.yaml are the node's keys until the database
// holds one, as at startup. Once it does they are never read again: a save
// with real keys is rejected, the example keys pass and are dropped, and a key
// written into the file by hand no longer serves after the next reload.
func TestConfigYAMLAPIKeysFollowWhetherTheDatabaseHoldsKeys(t *testing.T) {
	t.Run("no stored key yet", func(t *testing.T) {
		node := newDatabaseOwnedSectionsNode(t, nil)

		rec := putConfigYAML(node.h, []byte("port: 8319\napi-keys:\n  - "+yamlSaveShadowKey+"\n"))

		if rec.Code != http.StatusOK {
			t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
		}
		if !authenticatesOnNode(yamlSaveShadowKey) || !strings.Contains(readConfigFile(t, node.configPath), yamlSaveShadowKey) {
			t.Fatal("before the database holds a key, a config.yaml key must keep serving until startup imports it")
		}
	})
	t.Run("only the published example keys", func(t *testing.T) {
		node := newDatabaseOwnedSectionsNode(t, seedStoredAPIKey)

		rec := putConfigYAML(node.h, []byte("port: 8319\napi-keys:\n  - your-api-key-1\n"))

		if rec.Code != http.StatusOK {
			t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
		}
		if len(node.h.cfg.APIKeys) != 0 || strings.Contains(readConfigFile(t, node.configPath), "api-keys") {
			t.Fatalf("example keys should be dropped once the database holds keys: live=%v", node.h.cfg.APIKeys)
		}
		if !authenticatesOnNode(yamlSaveStoredKey) {
			t.Fatal("the stored key stopped authenticating")
		}
	})
	t.Run("written into the file by hand", func(t *testing.T) {
		node := newDatabaseOwnedSectionsNode(t, seedStoredAPIKey)
		if err := os.WriteFile(node.configPath, []byte("port: 8319\napi-keys:\n  - "+yamlSaveShadowKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// What the watcher does when config.yaml changes on disk.
		reloaded, err := config.LoadConfig(node.configPath)
		if err != nil {
			t.Fatal(err)
		}
		node.reload(reloaded)

		if authenticatesOnNode(yamlSaveShadowKey) || len(reloaded.APIKeys) != 0 {
			t.Fatalf("a key written into config.yaml served after a reload: live=%v", reloaded.APIKeys)
		}
		if data := readConfigFile(t, node.configPath); strings.Contains(data, yamlSaveShadowKey) || !strings.Contains(data, "port: 8319") {
			t.Fatalf("the reload should drop api-keys and keep the rest:\n%s", data)
		}
		if !authenticatesOnNode(yamlSaveStoredKey) {
			t.Fatal("the stored key stopped authenticating")
		}
	})
}

// Every offending section is named at once, so the operator fixes the file in
// one go, with sections edited on the same page grouped.
func TestPutConfigYAMLNamesEveryChangedDatabaseOwnedSection(t *testing.T) {
	node := newDatabaseOwnedSectionsNode(t, func(t *testing.T) {
		seedStoredRouting(t)
		seedStoredProxyPool(t)
		seedStoredAPIKey(t)
		seedStoredPermissionProfile(t)
	})
	body := "port: 8319\n" +
		"api-key-permission-profiles:\n  - id: yaml-profile\n    name: From YAML\n" +
		"api-keys:\n  - " + yamlSaveShadowKey + "\n" +
		"proxy-pool: []\n" +
		"api-key-entries:\n  - key: sk-yaml-entry-000000000004\n" +
		"routing:\n  include-default-group: false\n"

	rec := putConfigYAML(node.h, []byte(body))

	rejection := decodeDatabaseOwnedRejection(t, rec)
	want := []string{"routing", "proxy-pool", "api-keys", "api-key-entries", "api-key-permission-profiles"}
	if strings.Join(rejection.Details.Sections, ",") != strings.Join(want, ",") {
		t.Fatalf("rejected sections = %v, want %v", rejection.Details.Sections, want)
	}
	if !strings.Contains(rejection.Message, "api-keys and api-key-entries on the API Keys page") {
		t.Fatalf("message should group the API key sections: %q", rejection.Message)
	}
	assertFileContent(t, node.configPath, node.original)
}

// A stored section that cannot be read cannot be compared, and the save might
// change it; it is refused rather than let through. Saves that do not spell
// the section out are unaffected.
func TestPutConfigYAMLFailsClosedWhenAStoredSectionCannotBeRead(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T)
		corrupt string
		yaml    string
	}{
		{"routing", seedStoredRouting, `UPDATE routing_config SET payload = '{not json'`, "routing:\n  strategy: fill-first\n"},
		{"proxy-pool", seedStoredProxyPool, `UPDATE proxy_pool SET enabled = 'yes'`, "proxy-pool:\n  - id: hk-1\n    url: http://hk-1.example:8080\n    enabled: true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := newDatabaseOwnedSectionsNode(t, tc.seed)
			if _, err := usage.RuntimeDB().Exec(tc.corrupt); err != nil {
				t.Fatalf("corrupt the stored %s: %v", tc.name, err)
			}

			rec := putConfigYAML(node.h, []byte("port: 8319\nproxy-url: "+yamlSaveNewProxy+"\n"+tc.yaml))

			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "config.yaml was not saved") {
				t.Fatalf("PutConfigYAML status = %d body = %s, want 500", rec.Code, rec.Body.String())
			}
			assertStoredProxyURL(t, yamlSaveOldProxy)
			assertFileContent(t, node.configPath, node.original)

			rec = putConfigYAML(node.h, []byte("port: 8319\n"))
			if rec.Code != http.StatusOK {
				t.Fatalf("a save without %s: status = %d body = %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

const (
	yamlSaveStoredKey = "sk-stored-key-000000000001"
	yamlSaveShadowKey = "sk-shadow-key-000000000002"
)

// databaseOwnedSectionsNode is a node whose database owns proxy-url (see
// newRuntimeSettingYAMLSaveFixture), seeded with whatever a test stores, and
// whose config-mutated hook does to database-owned sections what production's
// does: applyConfigReload applies them from the database, then UpdateClients
// registers the API keys again.
type databaseOwnedSectionsNode struct {
	h          *Handler
	configPath string
	original   []byte
	hookCalls  int
}

func newDatabaseOwnedSectionsNode(t *testing.T, seed func(t *testing.T)) *databaseOwnedSectionsNode {
	t.Helper()
	h, configPath, original := newRuntimeSettingYAMLSaveFixture(t)
	if seed != nil {
		seed(t)
	}
	node := &databaseOwnedSectionsNode{h: h, configPath: configPath, original: original}
	t.Cleanup(func() { sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey) })
	// Startup loads config.yaml and applies the database over it. Like a real
	// start it stores the routing the file implies when none is stored: the
	// strategy defaults to round-robin, which counts as configured.
	live, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	node.reload(live)
	h.cfg = live
	h.SetConfigMutatedHook(func(cfg *config.Config) {
		node.hookCalls++
		node.reload(cfg)
	})
	return node
}

func (n *databaseOwnedSectionsNode) reload(cfg *config.Config) {
	serviceapp.ApplyDBBackedRuntimeSettings(cfg, n.configPath)
	configaccess.Register(&cfg.SDKConfig)
}

// authenticatesOnNode reports whether a client request with key passes the
// API key providers registered on this node.
func authenticatesOnNode(key string) bool {
	for _, provider := range sdkaccess.RegisteredProviders() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if result, err := provider.Authenticate(context.Background(), req); err == nil && result != nil {
			return true
		}
	}
	return false
}

type databaseOwnedRejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Sections []string `json:"sections"`
	} `json:"details"`
}

func decodeDatabaseOwnedRejection(t *testing.T, rec *httptest.ResponseRecorder) databaseOwnedRejection {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PutConfigYAML status = %d body = %s, want 422", rec.Code, rec.Body.String())
	}
	var body struct {
		Error databaseOwnedRejection `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "config_yaml_database_owned_sections" {
		t.Fatalf("rejection body = %s (%v), want code config_yaml_database_owned_sections", rec.Body.String(), err)
	}
	return body.Error
}

func storedRouting() config.RoutingConfig {
	return config.RoutingConfig{
		Strategy:            "fill-first",
		IncludeDefaultGroup: true,
		ChannelGroups:       []config.RoutingChannelGroup{{Name: "team-a", Match: config.ChannelGroupMatch{Prefixes: []string{"team-a"}}}},
		PathRoutes:          []config.RoutingPathRoute{{Path: "/team-a", Group: "team-a"}},
	}
}

func storedProxyPool() []config.ProxyPoolEntry {
	return []config.ProxyPoolEntry{
		{ID: "hk-1", Name: "HK 1", URL: "http://hk-1.example:8080", Enabled: true},
		{ID: "sg-1", Name: "SG 1", URL: "socks5://sg-1.example:1080", Enabled: false},
	}
}

func seedStoredRouting(t *testing.T) {
	t.Helper()
	if err := usage.UpsertRoutingConfig(storedRouting()); err != nil {
		t.Fatalf("seed routing: %v", err)
	}
}

func seedStoredProxyPool(t *testing.T) {
	t.Helper()
	if err := usage.ReplaceProxyPool(storedProxyPool()); err != nil {
		t.Fatalf("seed proxy pool: %v", err)
	}
}

func seedStoredAPIKey(t *testing.T) {
	t.Helper()
	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: yamlSaveStoredKey, Name: "stored"}); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
}

func seedStoredPermissionProfile(t *testing.T) {
	t.Helper()
	if err := usage.ReplaceAllAPIKeyPermissionProfiles([]usage.APIKeyPermissionProfileRow{{ID: "stored-profile", Name: "Stored"}}); err != nil {
		t.Fatalf("seed permission profile: %v", err)
	}
}

func assertStoredRoutingUnchanged(t *testing.T) {
	t.Helper()
	stored := usage.GetRoutingConfigForTenant(identity.SystemTenantID)
	if stored == nil || !config.SameRoutingConfig(*stored, storedRouting()) {
		t.Fatalf("stored routing = %+v, want the seeded routing", stored)
	}
}

func assertStoredProxyPoolUnchanged(t *testing.T) {
	t.Helper()
	if stored := usage.ListProxyPool(); !config.SameProxyPool(stored, storedProxyPool()) {
		t.Fatalf("stored proxy pool = %+v, want the seeded pool", stored)
	}
}

func assertShadowKeyNotStored(t *testing.T) {
	t.Helper()
	if usage.GetAPIKey(yamlSaveShadowKey) != nil || usage.GetAPIKey(yamlSaveStoredKey) == nil {
		t.Fatal("stored API keys changed")
	}
}

func assertStoredPermissionProfilesUnchanged(t *testing.T) {
	t.Helper()
	profiles := usage.ListAPIKeyPermissionProfiles()
	if len(profiles) != 1 || profiles[0].ID != "stored-profile" {
		t.Fatalf("stored permission profiles = %+v, want only stored-profile", profiles)
	}
}

func readConfigFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
