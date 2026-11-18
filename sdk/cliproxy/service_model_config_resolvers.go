package cliproxy

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func (s *Service) resolveConfigBedrockKey(auth *coreauth.Auth) *config.BedrockKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrAccessKeyID, attrBase, attrRegion, attrMode string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrAccessKeyID = strings.TrimSpace(auth.Attributes["access_key_id"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
		attrRegion = strings.TrimSpace(auth.Attributes["region"])
		attrMode = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_mode"]))
	}
	if attrMode == "apikey" || attrMode == "api_key" {
		attrMode = "api-key"
	}
	for i := range s.cfg.BedrockKey {
		entry := &s.cfg.BedrockKey[i]
		cfgMode := strings.ToLower(strings.TrimSpace(entry.AuthMode))
		if cfgMode == "" {
			cfgMode = "sigv4"
		}
		if cfgMode == "apikey" || cfgMode == "api_key" {
			cfgMode = "api-key"
		}
		if attrMode != "" && cfgMode != attrMode {
			continue
		}
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrBase != "" && !strings.EqualFold(cfgBase, attrBase) {
			continue
		}
		cfgRegion := strings.TrimSpace(entry.Region)
		if cfgRegion == "" {
			cfgRegion = config.DefaultBedrockRegion
		}
		if attrRegion != "" && !strings.EqualFold(cfgRegion, attrRegion) {
			continue
		}
		if cfgMode == "api-key" {
			if attrKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
			continue
		}
		cfgAccessKeyID := strings.TrimSpace(entry.AccessKeyID)
		if attrAccessKeyID != "" && strings.EqualFold(cfgAccessKeyID, attrAccessKeyID) {
			return entry
		}
		if attrKey != "" && strings.EqualFold(cfgAccessKeyID, attrKey) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigOpenCodeGoKey(auth *coreauth.Auth) *config.OpenCodeGoKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	for i := range s.cfg.OpenCodeGoKey {
		entry := &s.cfg.OpenCodeGoKey[i]
		if attrKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigClineKey(auth *coreauth.Auth) *config.ClineKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.ClineKey {
		entry := &s.cfg.ClineKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultClineBaseURL
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if attrBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigOllamaCloudKey(auth *coreauth.Auth) *config.OllamaCloudKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.OllamaCloudKey {
		entry := &s.cfg.OllamaCloudKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultOllamaCloudBaseURL
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if attrBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigCommandCodeKey(auth *coreauth.Auth) *config.CommandCodeKey {
	if auth == nil || s.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.CommandCodeKey {
		entry := &s.cfg.CommandCodeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultCommandCodeBaseURL
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if attrBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}
