package usage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	apiKeysMigrationBackupSuffix   = ".pre-sqlite-migration"
	proxyPoolMigrationBackupSuffix = ".pre-proxy-pool-sqlite-migration"
	routingMigrationBackupSuffix   = ".pre-routing-sqlite-migration"
	runtimeSettingsBackupSuffix    = ".pre-runtime-settings-sqlite-migration"
)

var dbBackedConfigYAMLKeys = map[string]bool{
	"api-keys":               true,
	"api-key-entries":        true,
	"routing":                true,
	"proxy-pool":             true,
	"gemini-api-key":         true,
	"codex-api-key":          true,
	"claude-api-key":         true,
	"bedrock-api-key":        true,
	"opencode-go-api-key":    true,
	"openai-compatibility":   true,
	"vertex-api-key":         true,
	"claude-header-defaults": true,
	"kimi-header-defaults":   true,
	"identity-fingerprint":   true,
	"oauth-excluded-models":  true,
	"oauth-model-alias":      true,
	"payload":                true,
}

// ConfigStoreAvailable reports whether the SQLite store that owns DB-backed
// config sections is ready. Callers must not remove YAML fallbacks when this is
// false.
func ConfigStoreAvailable() bool {
	return getDB() != nil
}

// CleanDBBackedConfigFromYAML removes config sections now owned by SQLite from
// config.yaml. It is safe to call repeatedly after management saves, because it
// only rewrites the file when one of the target root keys exists.
func CleanDBBackedConfigFromYAML(configFilePath string) int {
	return cleanConfigKeysFromYAML(configFilePath, dbBackedConfigYAMLKeys, "DB-backed config")
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
	cleanConfigKeysFromYAML(configFilePath, map[string]bool{
		"gemini-api-key":         true,
		"codex-api-key":          true,
		"claude-api-key":         true,
		"bedrock-api-key":        true,
		"opencode-go-api-key":    true,
		"openai-compatibility":   true,
		"vertex-api-key":         true,
		"claude-header-defaults": true,
		"kimi-header-defaults":   true,
		"identity-fingerprint":   true,
		"oauth-excluded-models":  true,
		"oauth-model-alias":      true,
		"payload":                true,
	}, "runtime_settings")
}

func cleanConfigKeysFromYAML(configFilePath string, keysToRemove map[string]bool, label string) int {
	if strings.TrimSpace(configFilePath) == "" || len(keysToRemove) == 0 {
		return 0
	}
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

	mode := os.FileMode(0o600)
	if info, err := os.Stat(configFilePath); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(configFilePath)
	base := filepath.Base(configFilePath)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, configFilePath)
}
