package usage

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	apiKeysMigrationBackupSuffix   = ".pre-sqlite-migration"
	proxyPoolMigrationBackupSuffix = ".pre-proxy-pool-sqlite-migration"
	routingMigrationBackupSuffix   = ".pre-routing-sqlite-migration"
	runtimeSettingsBackupSuffix    = ".pre-runtime-settings-sqlite-migration"
)

// runtimeSettingYAMLKeys are the config.yaml root keys owned by
// runtime_settings. Derived from the specs so a new database-backed setting is
// cleaned from config.yaml without a second list to keep in step.
func runtimeSettingYAMLKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, spec := range runtimeconfig.Specs() {
		keys[spec.Key] = true
	}
	return keys
}

// dbBackedConfigYAMLKeys are all config.yaml root keys owned by the database.
func dbBackedConfigYAMLKeys() map[string]bool {
	keys := runtimeSettingYAMLKeys()
	for _, key := range []string{"api-keys", "api-key-entries", "api-key-permission-profiles", "routing", "proxy-pool"} {
		keys[key] = true
	}
	return keys
}

// ConfigStoreAvailable reports whether the database store that owns DB-backed
// config sections is ready. Callers must not remove YAML fallbacks when this is
// false.
func ConfigStoreAvailable() bool {
	return getDB() != nil
}

// CleanDBBackedConfigFromYAML removes config sections now owned by the database from
// config.yaml. It is safe to call repeatedly after management saves, because it
// only rewrites the file when one of the target root keys exists.
func CleanDBBackedConfigFromYAML(configFilePath string) int {
	return cleanConfigKeysFromYAML(configFilePath, dbBackedConfigYAMLKeys(), "DB-backed config")
}

func backupConfigForMigration(configFilePath string, suffix string) bool {
	if strings.TrimSpace(configFilePath) == "" || strings.TrimSpace(suffix) == "" {
		return false
	}
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		log.Warnf("usage: failed to read config before migration backup: %v", err)
		return false
	}
	backupPath := configFilePath + suffix
	if _, statErr := os.Stat(backupPath); statErr == nil {
		// An earlier migration already left a backup under this name; it holds
		// the older, more complete config.yaml, so keep it and add a new one.
		backupPath = fmt.Sprintf("%s.%d", backupPath, time.Now().Unix())
	}
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		log.Warnf("usage: failed to backup config before cleanup: %v", err)
		return false
	}
	log.Infof("usage: backed up config.yaml to %s", backupPath)
	return true
}

func cleanAPIKeysFromYAML(configFilePath string) {
	cleanConfigKeysFromYAML(configFilePath, map[string]bool{
		"api-keys":        true,
		"api-key-entries": true,
	}, "api_keys")
}

func cleanProxyPoolFromYAML(configFilePath string) {
	cleanConfigKeysFromYAML(configFilePath, map[string]bool{
		"proxy-pool": true,
	}, "proxy_pool")
}

func cleanRoutingConfigFromYAML(configFilePath string) {
	cleanConfigKeysFromYAML(configFilePath, map[string]bool{
		"routing": true,
	}, "routing_config")
}

func cleanRuntimeSettingsFromYAML(configFilePath string) {
	cleanConfigKeysFromYAML(configFilePath, runtimeSettingYAMLKeys(), "runtime_settings")
}

// cleanConfigKeysFromYAML strips the given root keys from config.yaml. It runs under
// the shared config.yaml write lock: the cleanup is itself a read-modify-write cycle,
// and it is triggered both directly after management saves and from the watcher reload
// chain, so without the lock it can interleave with a management save and drop one
// side's changes.
func cleanConfigKeysFromYAML(configFilePath string, keysToRemove map[string]bool, label string) int {
	if strings.TrimSpace(configFilePath) == "" || len(keysToRemove) == 0 {
		return 0
	}
	removed := 0
	err := config.WithConfigFileWriteLock(func() error {
		removed = cleanConfigKeysFromYAMLLocked(configFilePath, keysToRemove, label)
		return nil
	})
	if err != nil {
		log.Warnf("usage: failed to acquire config write lock for %s cleanup: %v", label, err)
		return 0
	}
	return removed
}

func cleanConfigKeysFromYAMLLocked(configFilePath string, keysToRemove map[string]bool, label string) int {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		log.Warnf("usage: failed to read config for %s cleanup: %v", label, err)
		return 0
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		log.Warnf("usage: failed to parse config YAML for %s cleanup: %v", label, err)
		return 0
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return 0
	}
	mapNode := root.Content[0]
	if mapNode == nil || mapNode.Kind != yaml.MappingNode {
		return 0
	}

	filtered := make([]*yaml.Node, 0, len(mapNode.Content))
	removed := 0
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		keyNode := mapNode.Content[i]
		if keyNode != nil && keysToRemove[keyNode.Value] {
			removed++
			continue
		}
		filtered = append(filtered, mapNode.Content[i], mapNode.Content[i+1])
	}
	if removed == 0 {
		return 0
	}

	mapNode.Content = filtered
	if err := writeYAMLNodeAtomic(configFilePath, &root); err != nil {
		log.Warnf("usage: failed to write cleaned %s config: %v", label, err)
		return 0
	}
	log.Infof("usage: removed %d %s section(s) from config.yaml", removed, label)
	return removed
}

// writeYAMLNodeAtomic encodes root and hands it to the shared atomic writer in
// internal/config, so every config.yaml write goes through one implementation.
func writeYAMLNodeAtomic(configFilePath string, root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return config.WriteYAMLFileAtomic(configFilePath, buf.Bytes())
}
