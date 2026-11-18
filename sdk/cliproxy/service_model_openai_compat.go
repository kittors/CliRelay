package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func openAICompatAuthEntryActive(compat *config.OpenAICompatibility, auth *coreauth.Auth) bool {
	if compat == nil || compat.Disabled {
		return false
	}
	if len(compat.APIKeyEntries) == 0 {
		return true
	}
	authKey := ""
	if auth != nil && auth.Attributes != nil {
		authKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	for i := range compat.APIKeyEntries {
		entry := &compat.APIKeyEntries[i]
		if entry.Disabled {
			continue
		}
		if strings.TrimSpace(entry.APIKey) == authKey {
			return true
		}
	}
	return false
}

func (s *Service) registerOpenAICompatModels(
	auth *coreauth.Auth,
	provider string,
	compatProviderKey string,
	compatDisplayName string,
	compatDetected bool,
) bool {
	if s == nil || s.cfg == nil {
		return false
	}

	providerKey := provider
	compatName := ""
	if auth != nil {
		compatName = strings.TrimSpace(auth.Provider)
	}
	isCompatAuth := false
	if compatDetected {
		if compatProviderKey != "" {
			providerKey = compatProviderKey
		}
		if compatDisplayName != "" {
			compatName = compatDisplayName
		}
		isCompatAuth = true
	}
	if strings.EqualFold(providerKey, "openai-compatibility") {
		isCompatAuth = true
		if auth != nil && auth.Attributes != nil {
			if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
				compatName = v
			}
			if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
				providerKey = strings.ToLower(v)
			}
		}
		if providerKey == "openai-compatibility" && compatName != "" {
			providerKey = strings.ToLower(compatName)
		}
	} else if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			compatName = v
			isCompatAuth = true
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			providerKey = strings.ToLower(v)
			isCompatAuth = true
		}
	}

	for i := range s.cfg.OpenAICompatibility {
		compat := &s.cfg.OpenAICompatibility[i]
		if !strings.EqualFold(compat.Name, compatName) {
			continue
		}
		if !openAICompatAuthEntryActive(compat, auth) {
			if auth != nil {
				GlobalModelRegistry().UnregisterClient(auth.ID)
			}
			return true
		}
		ms := make([]*ModelInfo, 0, len(compat.Models))
		for j := range compat.Models {
			m := compat.Models[j]
			modelID := m.Alias
			if modelID == "" {
				modelID = m.Name
			}
			ms = append(ms, &ModelInfo{
				ID:          modelID,
				Object:      "model",
				Created:     time.Now().Unix(),
				OwnedBy:     compat.Name,
				Type:        "openai-compatibility",
				DisplayName: modelID,
				UserDefined: true,
			})
		}
		if auth == nil {
			return true
		}
		if len(ms) > 0 {
			if providerKey == "" {
				providerKey = "openai-compatibility"
			}
			GlobalModelRegistry().RegisterClient(auth.ID, providerKey, applyModelPrefixes(ms, auth.Prefix, s.cfg.ForceModelPrefix))
		} else {
			GlobalModelRegistry().UnregisterClient(auth.ID)
		}
		return true
	}

	if isCompatAuth && auth != nil {
		GlobalModelRegistry().UnregisterClient(auth.ID)
		return true
	}
	return false
}
