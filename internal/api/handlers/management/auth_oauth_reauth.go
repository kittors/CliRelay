package management

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cleanupSupersededReauthAliases removes older credential files belonging to
// the same physical upstream account in tenantID after the replacement
// credential has been durably saved and registered.
//
// Channel references shared by the new credential (such as email or inherited
// label) are intentionally preserved; only identifiers unique to the superseded
// alias are renamed to the new credential's channel name before removal.
func (h *Handler) cleanupSupersededReauthAliases(ctx context.Context, tenantID string, replacement *coreauth.Auth, aliases []*coreauth.Auth) {
	if h == nil || h.cfg == nil || replacement == nil || len(aliases) == 0 {
		return
	}
	tenantID = managementauthfiles.NormalizeTenantID(tenantID)
	newChannels := authChannelIdentifierSet(replacement)
	targetChannel := strings.TrimSpace(replacement.ChannelName())

	for _, alias := range aliases {
		if alias == nil || alias.ID == replacement.ID {
			continue
		}
		if targetChannel != "" {
			if uniqueOld := uniqueAliasChannels(alias, newChannels); len(uniqueOld) > 0 {
				if err := h.renameChannelReferencesForTenant(tenantID, uniqueOld, targetChannel); err != nil {
					log.WithError(err).Warnf("reauth cleanup: failed to migrate channel references from %s to %s", alias.ID, replacement.ID)
				}
			}
		}
		h.migrateSharedAuthGrantsOnReplace(ctx, tenantID, alias.ID, replacement.ID)

		aliasName := supersededAliasFileName(alias)
		if aliasName == "" {
			continue
		}
		deleteSvc := managementauthfiles.DeleteService{
			AuthDir:    h.cfg.AuthDir,
			TenantID:   tenantID,
			Manager:    h.authManager,
			Repository: h.authFileRepository(),
			// Never pass RemoveChannels here: the replacement credential still
			// serves the same account's channel name / email.
			RemoveAuthBindings: func(authIDs []string) error {
				h.dropSharedAuthGrants(ctx, tenantID, authIDs)
				return h.deleteContentModerationBindings(ctx, tenantID, contentmoderation.ChannelTypeAuthFile, authIDs)
			},
		}
		if _, err := deleteSvc.DeleteOne(ctx, aliasName); err != nil {
			log.WithError(err).Warnf("reauth cleanup: failed to remove superseded alias %s replaced by %s", alias.ID, replacement.ID)
		} else {
			log.Infof("reauth cleanup: removed superseded credential alias %s replaced by %s", alias.ID, replacement.ID)
		}
	}
}

func supersededAliasFileName(alias *coreauth.Auth) string {
	if alias == nil {
		return ""
	}
	if name := managementauthfiles.PublicFileName(alias); name != "" {
		return name
	}
	for _, candidate := range []string{alias.FileName, alias.ID} {
		base := filepath.Base(strings.TrimSpace(candidate))
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	return ""
}

func authChannelIdentifierSet(auth *coreauth.Auth) map[string]struct{} {
	out := make(map[string]struct{})
	if auth == nil {
		return out
	}
	for _, name := range managementauthfiles.DeletedChannelIdentifiers(auth) {
		if key := strings.ToLower(strings.TrimSpace(name)); key != "" {
			out[key] = struct{}{}
		}
	}
	if ch := strings.ToLower(strings.TrimSpace(auth.ChannelName())); ch != "" {
		out[ch] = struct{}{}
	}
	return out
}

func uniqueAliasChannels(alias *coreauth.Auth, keep map[string]struct{}) []string {
	if alias == nil {
		return nil
	}
	var unique []string
	seen := make(map[string]struct{})
	for _, name := range managementauthfiles.DeletedChannelIdentifiers(alias) {
		trimmed := strings.TrimSpace(name)
		key := strings.ToLower(trimmed)
		if key == "" {
			continue
		}
		if _, exists := keep[key]; exists {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}

func (h *Handler) migrateSharedAuthGrantsOnReplace(ctx context.Context, tenantID, oldAuthID, newAuthID string) {
	if tenantID != identity.SystemTenantID || strings.TrimSpace(oldAuthID) == "" || strings.TrimSpace(newAuthID) == "" || oldAuthID == newAuthID {
		return
	}
	if !usage.ResourceGrantsAvailable() {
		return
	}
	grants, err := usage.ListResourceGrants(ctx, resourcegrants.KindAuth)
	if err != nil {
		log.WithError(err).Warn("reauth cleanup: read shared credential grants before replacing alias")
		return
	}
	oldGrantees := grants[oldAuthID]
	if len(oldGrantees) == 0 {
		return
	}
	mergedSet := make(map[string]struct{}, len(oldGrantees)+len(grants[newAuthID]))
	for _, g := range grants[newAuthID] {
		if trimmed := strings.TrimSpace(g); trimmed != "" {
			mergedSet[trimmed] = struct{}{}
		}
	}
	for _, g := range oldGrantees {
		if trimmed := strings.TrimSpace(g); trimmed != "" {
			mergedSet[trimmed] = struct{}{}
		}
	}
	merged := make([]string, 0, len(mergedSet))
	for g := range mergedSet {
		merged = append(merged, g)
	}
	sort.Strings(merged)
	if _, err := usage.ReplaceResourceGrants(ctx, resourcegrants.KindAuth, []string{newAuthID}, merged); err != nil {
		log.WithError(err).Warnf("reauth cleanup: migrate shared credential grants from %s to %s", oldAuthID, newAuthID)
		return
	}
	h.reloadSharedAuthGrants()
}
