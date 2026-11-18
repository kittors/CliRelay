package modelcatalog

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"strings"
)

func (s *Service) managementAuthoritativeModelKeys() map[string]bool {
	rows, ownerKeys, _, ok := s.defaultMappedOwnerRows()
	if !ok {
		return nil
	}
	return mappedOwnerRowModelKeys(rows, ownerKeys)
}

func sourceHasExplicitConfigModels(source registry.ModelClientSource, authByID map[string]*coreauth.Auth) bool {
	auth := authByID[strings.TrimSpace(source.ClientID)]
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey") {
		return false
	}
	if strings.TrimSpace(auth.Attributes["models_hash"]) == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.Attributes["source"])), "config:")
}
