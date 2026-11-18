package usage

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// databaseOwnedSectionOrder lists the config.yaml root sections the database
// owns besides the runtime settings, in the order a rejected save names them.
// Runtime settings are different: a management save stores them (see
// PersistRuntimeSettingsPresentInYAML) instead of being checked here.
var databaseOwnedSectionOrder = []string{"routing", "proxy-pool", "api-keys", "api-key-entries", "api-key-permission-profiles"}

// databaseOwnedSectionHomes says where each of those sections is edited once
// the database holds it: the panel page and the management endpoint, when a
// single one replaces the section.
var databaseOwnedSectionHomes = map[string]string{
	"routing":                     "the Channel Groups page (PUT /v0/management/routing-config)",
	"proxy-pool":                  "the Outbound Proxies page (PUT /v0/management/proxy-pool)",
	"api-keys":                    "the API Keys page",
	"api-key-entries":             "the API Keys page",
	"api-key-permission-profiles": "the Portal Account Permissions page (PUT /v0/management/api-key-permission-profiles)",
}

// DatabaseOwnedSection is a database-owned config.yaml root section that a
// management save spelled out with a value the database would discard.
type DatabaseOwnedSection struct {
	// Key is the config.yaml root key.
	Key string
	// Home says where the section is edited instead.
	Home string
}

// DatabaseOwnedSectionsChangedByYAML returns the database-owned root sections
// that yamlContent, a management config.yaml save that loaded into cfg, spells
// out with a value the database would discard. Once the database holds a
// section it owns it: the reload after the save applies the stored value and
// drops the section from config.yaml, so saving a different value would report
// success while the change existed nowhere. API keys were worse off: a reload
// kept them as keys of this node alone (see DropYAMLAPIKeysWhenStored).
//
//   - routing and proxy-pool count when they differ from the stored value,
//     both normalized. A copy of the stored value passes: the panel's visual
//     editor sends back a routing section it found in the file.
//   - api-keys, api-key-entries and api-key-permission-profiles count when
//     they hold anything a first import would take. The file cannot carry what
//     the stored rows hold (key IDs, owners, tenants, quotas), so no such
//     value is a copy of them.
//
// A section the database does not hold yet is left to its first import, by
// the reload after the save (routing, proxy-pool) or by the next start (the
// others), as before. canonicalizeRouting, when set, is applied to both
// routing values first, as the routing-config endpoint does before storing.
// An error means a stored section could not be read; the save has to stop,
// since a section the database does hold would be discarded.
func DatabaseOwnedSectionsChangedByYAML(ctx context.Context, cfg *config.Config, yamlContent []byte, canonicalizeRouting func(config.RoutingConfig) config.RoutingConfig) ([]DatabaseOwnedSection, error) {
	if cfg == nil || !ConfigStoreAvailable() {
		return nil, nil
	}
	present := config.YAMLRootKeys(yamlContent)
	var changed []DatabaseOwnedSection
	for _, key := range databaseOwnedSectionOrder {
		if !present[key] {
			continue
		}
		differs, err := databaseOwnedSectionDiffers(ctx, key, cfg, yamlContent, canonicalizeRouting)
		if err != nil {
			return nil, fmt.Errorf("compare config.yaml %s with the database: %w", key, err)
		}
		if differs {
			changed = append(changed, DatabaseOwnedSection{Key: key, Home: databaseOwnedSectionHomes[key]})
		}
	}
	return changed, nil
}

func databaseOwnedSectionDiffers(ctx context.Context, key string, cfg *config.Config, yamlContent []byte, canonicalizeRouting func(config.RoutingConfig) config.RoutingConfig) (bool, error) {
	switch key {
	case "routing":
		stored, err := LoadStoredRoutingConfig(ctx)
		if err != nil || stored == nil {
			return false, err
		}
		written, current := cfg.Routing, *stored
		if canonicalizeRouting != nil {
			written, current = canonicalizeRouting(written), canonicalizeRouting(current)
		}
		return !config.SameRoutingConfig(written, current), nil
	case "proxy-pool":
		stored, err := LoadStoredProxyPool(ctx)
		if err != nil || len(stored) == 0 {
			return false, err
		}
		return !config.SameProxyPool(cfg.ProxyPool, stored), nil
	case "api-keys", "api-key-entries":
		if !yamlAPIKeySectionHoldsKeys(key, cfg) {
			return false, nil
		}
		return APIKeysStored(ctx)
	case "api-key-permission-profiles":
		if !yamlHoldsPermissionProfiles(yamlContent) {
			return false, nil
		}
		return APIKeyPermissionProfilesStored(ctx)
	default:
		return false, fmt.Errorf("no check for database-owned section %q", key)
	}
}

// yamlAPIKeySectionHoldsKeys reports whether the api-keys or api-key-entries
// section of cfg holds a key a first import would take: not blank and not one
// of the published example keys, which are never imported and are left in
// config.yaml on purpose (see MigrateAPIKeysFromConfig).
func yamlAPIKeySectionHoldsKeys(key string, cfg *config.Config) bool {
	var values []string
	switch key {
	case "api-keys":
		values = cfg.APIKeys
	case "api-key-entries":
		for _, entry := range cfg.APIKeyEntries {
			values = append(values, entry.Key)
		}
	}
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" && !config.IsPlaceholderAPIKey(trimmed) {
			return true
		}
	}
	return false
}

// yamlHoldsPermissionProfiles reports whether api-key-permission-profiles in
// yamlContent holds a profile a first import would take, one with an ID and a
// name (see MigrateAPIKeyPermissionProfilesFromYAML). A section that does not
// decode counts as holding one: nothing would ever apply it.
func yamlHoldsPermissionProfiles(yamlContent []byte) bool {
	var root struct {
		Profiles []APIKeyPermissionProfileRow `yaml:"api-key-permission-profiles"`
	}
	if err := yaml.Unmarshal(yamlContent, &root); err != nil {
		return true
	}
	for _, profile := range root.Profiles {
		profile = normalizeAPIKeyPermissionProfile(profile)
		if profile.ID != "" && profile.Name != "" {
			return true
		}
	}
	return false
}

// DropYAMLAPIKeysWhenStored does at every reload what MigrateAPIKeysFromConfig
// does at startup once the database holds API keys: it drops api-keys and
// api-key-entries from cfg and from config.yaml. A reload used to keep them in
// cfg, where the access provider accepts them as fallback keys, so a key
// written into config.yaml worked on this node alone: without a key ID, absent
// from the panel, unknown to the other nodes and gone after the next start.
// While the database holds no key they stay, as at startup: they are the
// node's keys until the next start imports them, and the published example
// keys among them keep allow-unauthenticated from opening the node. They also
// stay when the database cannot answer, for the same reason.
func DropYAMLAPIKeysWhenStored(cfg *config.Config, configFilePath string) bool {
	if cfg == nil || (len(cfg.APIKeys) == 0 && len(cfg.APIKeyEntries) == 0) || !ConfigStoreAvailable() {
		return false
	}
	stored, err := APIKeysStored(context.Background())
	if err != nil {
		log.Warnf("usage: kept the api-keys of config.yaml for now: could not tell whether the database holds API keys: %v", err)
		return false
	}
	if !stored {
		return false
	}
	dropYAMLAPIKeys(cfg, configFilePath)
	return true
}

// dropYAMLAPIKeys removes api-keys and api-key-entries from cfg and from
// config.yaml once the database owns API keys. Only real keys are warned
// about; the published example keys were never meant to be used.
func dropYAMLAPIKeys(cfg *config.Config, configFilePath string) {
	held := yamlAPIKeySectionHoldsKeys("api-keys", cfg) || yamlAPIKeySectionHoldsKeys("api-key-entries", cfg)
	cfg.APIKeys = nil
	cfg.APIKeyEntries = nil
	if cleanAPIKeysFromYAML(configFilePath) > 0 && held {
		log.Warnf("usage: ignored the api-keys and api-key-entries of config.yaml: the database holds API keys and owns them; create keys on %s", databaseOwnedSectionHomes["api-keys"])
	}
}
