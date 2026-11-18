package config

import (
	"path/filepath"
	"strings"
)

const (
	DefaultAuthDir     = "~/.cli-proxy-api"
	DefaultKVStorePath = "kv.db"
)

// KVStoreConfig controls the local persistent key-value store.
type KVStoreConfig struct {
	Disabled bool   `yaml:"disabled" json:"disabled"`
	Path     string `yaml:"path,omitempty" json:"path,omitempty"`
}

func (c KVStoreConfig) EffectivePath() string {
	if path := strings.TrimSpace(c.Path); path != "" {
		return path
	}
	return DefaultKVStorePath
}

func (c KVStoreConfig) EffectivePathForAuthDir(authDir string) string {
	if path := strings.TrimSpace(c.Path); path != "" {
		return path
	}
	base := strings.TrimSpace(authDir)
	if base == "" {
		base = DefaultAuthDir
	}
	return filepath.Join(base, DefaultKVStorePath)
}

func (cfg *Config) EffectiveKVStorePath() string {
	if cfg == nil {
		return filepath.Join(DefaultAuthDir, DefaultKVStorePath)
	}
	return cfg.KVStore.EffectivePathForAuthDir(cfg.AuthDir)
}
