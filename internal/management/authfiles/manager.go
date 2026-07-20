package authfiles

import (
	"context"
	"path/filepath"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// PublicFileName returns the tenant-local filename exposed by management APIs.
// Auth IDs stay tenant-scoped (for example <tenant-id>/account.json), while
// mutation endpoints intentionally accept only a single safe filename.
func PublicFileName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = strings.TrimSpace(auth.ID)
	}
	return singleFileBaseName(name)
}

func FindByNameOrID(manager *coreauth.Manager, name string) *coreauth.Auth {
	return FindByNameOrIDForTenant(manager, "", name)
}

func FindByNameOrIDForTenant(manager *coreauth.Manager, tenantID, name string) *coreauth.Auth {
	if manager == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	tenantID = NormalizeTenantID(tenantID)
	if auth, ok := manager.GetByID(name); ok && coreauth.NormalizedTenantID(auth.TenantID) == tenantID {
		return auth
	}
	for _, auth := range manager.ListForTenant(tenantID) {
		if auth == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(auth.FileName), name) {
			return auth
		}
		if path := strings.TrimSpace(Attribute(auth, "path")); path != "" && strings.EqualFold(filepath.Base(path), name) {
			return auth
		}
	}
	return nil
}

func DeletedChannelIdentifiers(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	accountType, _ := auth.AccountInfo()
	if !strings.EqualFold(accountType, "oauth") {
		return nil
	}
	return auth.ChannelIdentifiers()
}

func RemoveFromManager(ctx context.Context, manager *coreauth.Manager, authDir, id string) {
	if manager == nil {
		return
	}
	trimmedID := strings.TrimSpace(id)
	candidates := []string{
		AuthIDForPath(authDir, trimmedID),
		trimmedID,
		filepath.Base(trimmedID),
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "." {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if deleted, _ := manager.Delete(coreauth.WithSkipPersist(ctx), candidate); deleted != nil {
			return
		}
	}
	if auth := FindByNameOrID(manager, filepath.Base(trimmedID)); auth != nil {
		_, _ = manager.Delete(coreauth.WithSkipPersist(ctx), auth.ID)
	}
}
