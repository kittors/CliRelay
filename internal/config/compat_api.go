package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"gopkg.in/yaml.v3"
)

// ComputeSourceHash returns the stable ETag digest used by management config
// endpoints.
func ComputeSourceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ParseConfigBytes parses a configuration document without touching disk.
func ParseConfigBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.SanitizeOpenAICompatibility()
	cfg.SanitizeOAuthModelAlias()
	return &cfg, nil
}

// NormalizeConfigLayout applies v8 aliases and returns the normalized YAML.
func NormalizeConfigLayout(data []byte, migrate bool) ([]byte, bool, error) {
	if !migrate {
		return data, false, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, false, err
	}
	if err := NormalizeV8ConfigAliases(&root); err != nil {
		return nil, false, err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		_ = enc.Close()
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	normalized := out.Bytes()
	return normalized, !bytes.Equal(normalized, data), nil
}

func NormalizeCollectionNodeStyles(_ *yaml.Node) {}

func ValidateV8Config(data []byte) error {
	_, err := ParseConfigBytes(data)
	return err
}
