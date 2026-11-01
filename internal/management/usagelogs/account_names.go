package usagelogs

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// channelLabelForTenant is how tenantID's log views name auth: a platform
// account shared with the tenant goes by its stand-in name
// (coreauth.Auth.SharedName), never by its login.
func channelLabelForTenant(auth *coreauth.Auth, tenantID string) string {
	if auth.SharedWith(tenantID) {
		return auth.SharedName()
	}
	return strings.TrimSpace(auth.ChannelName())
}

// authIndexAliasGroup returns the live EnsureIndex first, followed by known
// historical seeds for the same credential file. Path-format churn
// (basename vs tenant-relative FileName, id: vs file: seed) must not split one
// account across multiple channel options.
func authIndexAliasGroup(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	primary := strings.TrimSpace(auth.EnsureIndex())
	if primary == "" {
		return nil
	}

	seen := map[string]struct{}{primary: {}}
	out := []string{primary}
	addSeed := func(seed string) {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			return
		}
		idx := authIndexFromSeed(seed)
		if idx == "" {
			return
		}
		if _, ok := seen[idx]; ok {
			return
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}

	fileName := strings.TrimSpace(auth.FileName)
	if fileName != "" {
		base := filepath.Base(fileName)
		// Current and historical FileName forms.
		addSeed("file:" + fileName)
		addSeed("file:" + base)
		// Historical path: ID was set to the auth file name (including .json)
		// before FileName was populated, so EnsureIndex used id:<filename>.
		addSeed("id:" + fileName)
		addSeed("id:" + base)
		// Path-format churn: basename <-> tenant-relative.
		if tenantID := strings.TrimSpace(auth.TenantID); tenantID != "" && base != "" {
			addSeed("file:" + tenantID + "/" + base)
			addSeed("id:" + tenantID + "/" + base)
		}
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		addSeed("id:" + id)
		base := filepath.Base(id)
		addSeed("id:" + base)
		// If ID is a bare filename / tenant-relative path, also cover file: variants.
		if strings.HasSuffix(strings.ToLower(base), ".json") {
			addSeed("file:" + id)
			addSeed("file:" + base)
			if tenantID := strings.TrimSpace(auth.TenantID); tenantID != "" {
				addSeed("file:" + tenantID + "/" + base)
				addSeed("id:" + tenantID + "/" + base)
			}
		}
	}
	return out
}

// xaiOAuthAuthIndexGroup is kept for tests that assert the historical xAI
// id:/file: seed merge; production uses authIndexAliasGroup for all providers.
func xaiOAuthAuthIndexGroup(auth *coreauth.Auth) []string {
	return authIndexAliasGroup(auth)
}

func isXAIProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "xai", "grok":
		return true
	default:
		return false
	}
}

// authIndexFromSeed mirrors coreauth.stableAuthIndex (sha256 first 8 bytes hex).
func authIndexFromSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}
