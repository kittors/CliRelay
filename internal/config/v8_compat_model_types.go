package config

import "strings"

// SensitivePatternRule defines an ordered regular-expression replacement rule.
type SensitivePatternRule struct {
	Pattern     string `yaml:"pattern" json:"pattern"`
	Replacement string `yaml:"replacement" json:"replacement"`
}

// ProxyConnectionPoolConfig configures upstream connection reuse for a credential.
type ProxyConnectionPoolConfig struct {
	Enabled             *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	IdleConnTimeout     string `yaml:"idle-conn-timeout,omitempty" json:"idle-conn-timeout,omitempty"`
	MaxIdleConnsPerHost *int   `yaml:"max-idle-conns-per-host,omitempty" json:"max-idle-conns-per-host,omitempty"`
}

// OAuthModelSetting configures a model override for OAuth-backed credentials.
type OAuthModelSetting struct {
	OpenAPI          *bool  `yaml:"openapi,omitempty" json:"openapi,omitempty"`
	Name             string `yaml:"name" json:"name"`
	Alias            string `yaml:"alias,omitempty" json:"alias,omitempty"`
	MaxContextLength int    `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`
}

func (s OAuthModelSetting) GetMaxContextLength() int { return s.MaxContextLength }

// ResolveOAuthModelSetting finds the most specific setting for a model.
func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, metadataModelID, modelName string) *OAuthModelSetting {
	id := strings.ToLower(strings.TrimSpace(modelID))
	metaID := strings.ToLower(strings.TrimSpace(metadataModelID))
	name := strings.ToLower(strings.TrimSpace(modelName))
	var aliasMatch, nameMatch *OAuthModelSetting
	for i := range settings {
		entry := &settings[i]
		entryName := strings.ToLower(strings.TrimSpace(entry.Name))
		if entryName == "" {
			continue
		}
		entryAlias := strings.ToLower(strings.TrimSpace(entry.Alias))
		if entryAlias != "" && id != "" && id == entryAlias {
			aliasMatch = entry
			continue
		}
		if (entryAlias == "" || entryAlias == id) &&
			(id == entryName || (metaID != "" && metaID == entryName) || (name != "" && name == entryName)) {
			nameMatch = entry
		}
	}
	if aliasMatch != nil {
		return aliasMatch
	}
	return nameMatch
}
