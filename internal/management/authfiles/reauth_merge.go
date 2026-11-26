package authfiles

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ReauthReplacementPlan describes existing tenant credentials that an OAuth
// login or credential import is replacing: an exact same-ID entry (if any) and
// any older filename aliases for the same physical upstream account (for
// example codex-user-go.json superseded by codex-user-pro.json after a plan
// upgrade).
type ReauthReplacementPlan struct {
	SameID            *coreauth.Auth
	SupersededAliases []*coreauth.Auth
}

// FindReauthReplacementPlan inspects the tenant's existing credentials and
// finds the same-ID entry and any superseded filename aliases that represent
// the exact same physical upstream account.
//
// Matching is strictly scoped to tenantID and provider, skips runtime-only and
// API-key credentials, and only matches across filenames when both credentials
// resolve to a stable physical account identity (account_user_id, account_id,
// or email) — never falling back to filename/auth_id/auth_index.
func FindReauthReplacementPlan(manager *coreauth.Manager, tenantID string, incoming *coreauth.Auth) ReauthReplacementPlan {
	var plan ReauthReplacementPlan
	if manager == nil || incoming == nil {
		return plan
	}
	tenantID = NormalizeTenantID(tenantID)
	incomingProvider := strings.ToLower(strings.TrimSpace(incoming.Provider))
	incomingIdentity := usage.ResolveAuthSubjectIdentity(incoming)

	for _, existing := range manager.ListForTenant(tenantID) {
		if existing == nil || IsRuntimeOnly(existing) {
			continue
		}
		if sameCredentialID(existing, incoming) {
			plan.SameID = existing.Clone()
			continue
		}
		if incomingProvider == "" || !strings.EqualFold(strings.TrimSpace(existing.Provider), incomingProvider) {
			continue
		}
		if isAPIKeyCredential(existing) {
			continue
		}
		if samePhysicalOAuthAccount(incoming, incomingIdentity, existing) {
			plan.SupersededAliases = append(plan.SupersededAliases, existing.Clone())
		}
	}

	sortReplacementSources(plan.SupersededAliases)
	return plan
}

// ApplyReauthOperationalInheritance copies operator-managed settings (proxy
// binding, tags, concurrency, Codex options, subscription period, custom label,
// etc.) from the replaced credential(s) onto incoming without carrying over
// stale tokens, disabled flags, or runtime error/health state.
//
// Any setting explicitly chosen during the new login/import (such as a newly
// selected proxy_id) takes precedence over the inherited value.
func ApplyReauthOperationalInheritance(incoming *coreauth.Auth, plan ReauthReplacementPlan) {
	if incoming == nil {
		return
	}
	if plan.SameID != nil {
		InheritOperationalSettings(incoming, plan.SameID)
	}
	for _, alias := range plan.SupersededAliases {
		InheritOperationalSettings(incoming, alias)
	}
	// A fresh login/import always begins active and clean.
	incoming.Disabled = false
	incoming.Status = coreauth.StatusActive
	incoming.StatusMessage = ""
	incoming.Unavailable = false
	incoming.LastError = nil
	incoming.NextRetryAfter = time.Time{}
	incoming.NextRefreshAfter = time.Time{}
	incoming.ModelStates = nil
	if incoming.Metadata != nil {
		delete(incoming.Metadata, coreauth.DisabledMetadataKey)
		delete(incoming.Metadata, coreauth.CodexOAuthHealthMetadataKey)
		delete(incoming.Metadata, coreauth.ClaudeOAuthHealthMetadataKey)
		delete(incoming.Metadata, coreauth.CredentialVersionMetadataKey)
	}
}

// InheritOperationalSettings copies whitelisted operator-configured fields from
// src onto dst when dst does not already specify them. Never copies token
// material, disabled state, or runtime failure/health observations.
func InheritOperationalSettings(dst, src *coreauth.Auth) {
	if dst == nil || src == nil {
		return
	}
	dstMeta := ensureMetadata(dst)
	dstAttrs := ensureAttributes(dst)
	srcMeta := src.Metadata

	// Proxy binding: explicit proxy_id on dst wins (the operator picked a new
	// proxy in the login/import dialog). Only fall back to src when dst left
	// proxy unset.
	dstProxyID := firstNonEmptyTrimmed(dst.ProxyID, MetadataString(dstMeta, "proxy_id", "proxy-id", "proxyId"))
	dstProxyURL := firstNonEmptyTrimmed(dst.ProxyURL, MetadataString(dstMeta, "proxy_url", "proxy-url", "proxyUrl"))
	if dstProxyID == "" {
		if srcProxyID := firstNonEmptyTrimmed(src.ProxyID, MetadataString(srcMeta, "proxy_id", "proxy-id", "proxyId")); srcProxyID != "" {
			dst.ProxyID = srcProxyID
			dstMeta["proxy_id"] = srcProxyID
			dstProxyID = srcProxyID
		}
	}
	if dstProxyID == "" && dstProxyURL == "" {
		if srcProxyURL := firstNonEmptyTrimmed(src.ProxyURL, MetadataString(srcMeta, "proxy_url", "proxy-url", "proxyUrl")); srcProxyURL != "" {
			dst.ProxyURL = srcProxyURL
			dstMeta["proxy_url"] = srcProxyURL
		}
	}

	if strings.TrimSpace(dst.Prefix) == "" && MetadataString(dstMeta, "prefix") == "" {
		if prefix := firstNonEmptyTrimmed(src.Prefix, MetadataString(srcMeta, "prefix")); prefix != "" {
			dst.Prefix = prefix
			dstMeta["prefix"] = prefix
		}
	}

	// Custom channel label: only inherit when the operator explicitly set
	// metadata["label"] on src and dst does not already carry one.
	if MetadataString(dstMeta, "label") == "" {
		if customLabel := MetadataString(srcMeta, "label"); customLabel != "" {
			dst.Label = customLabel
			dstMeta["label"] = customLabel
		}
	}

	inheritStringSliceMetadata(dstMeta, srcMeta, "custom_tags")
	inheritStringSliceMetadata(dstMeta, srcMeta, "hidden_default_tags")
	inheritStringSliceMetadata(dstMeta, srcMeta, "display_tags")

	if _, exists := dstMeta["priority"]; !exists {
		if priority, ok := metadataInt(srcMeta, "priority"); ok && priority != 0 {
			dstMeta["priority"] = priority
		}
	}

	inheritSubscriptionMetadata(dstMeta, srcMeta)

	if _, hasLimit := metadataInt(dstMeta, "concurrency_limit", "concurrency", "concurrency-limit"); !hasLimit {
		if limit, ok := metadataInt(srcMeta, "concurrency_limit", "concurrency", "concurrency-limit"); ok && limit > 0 {
			dstMeta["concurrency_limit"] = limit
			dstAttrs["concurrency_limit"] = strconv.Itoa(limit)
		}
	}

	if _, ok := MetadataBoolPresence(dstMeta, "disable_cooling", "disable-cooling"); !ok {
		if disableCooling, has := MetadataBoolPresence(srcMeta, "disable_cooling", "disable-cooling"); has {
			dstMeta["disable_cooling"] = disableCooling
		}
	}

	if strings.EqualFold(strings.TrimSpace(dst.Provider), "codex") {
		if MetadataString(dstMeta, metadataKeyCodexConvergenceMode, "convergence_mode", "codex-convergence-mode") == "" {
			if mode := MetadataString(srcMeta, metadataKeyCodexConvergenceMode, "convergence_mode", "codex-convergence-mode"); mode != "" {
				dstMeta[metadataKeyCodexConvergenceMode] = strings.ToLower(mode)
				dstAttrs[metadataKeyCodexConvergenceMode] = strings.ToLower(mode)
			}
		}
		if MetadataString(dstMeta, metadataKeyCodexServiceTier, "service_tier", "codex-service-tier") == "" {
			if tier := MetadataString(srcMeta, metadataKeyCodexServiceTier, "service_tier", "codex-service-tier"); tier != "" {
				if norm := NormalizeCodexServiceTier(tier); norm != "" && norm != "default" {
					dstMeta[metadataKeyCodexServiceTier] = norm
					dstAttrs[metadataKeyCodexServiceTier] = norm
				}
			}
		}
		if _, ok := MetadataBoolPresence(dstMeta, "codex_cli_only"); !ok {
			if cliOnly, has := MetadataBoolPresence(srcMeta, "codex_cli_only"); has {
				dstMeta["codex_cli_only"] = cliOnly
			}
		}
		inheritStringSliceMetadata(dstMeta, srcMeta, "codex_cli_only_allowed_clients")
		if _, ok := MetadataBoolPresence(dstMeta, metadataKeyCodexImageGenerationBridge); !ok {
			if bridge, has := MetadataBoolPresence(srcMeta, metadataKeyCodexImageGenerationBridge); has {
				dstMeta[metadataKeyCodexImageGenerationBridge] = bridge
			}
		}
		if MetadataString(dstMeta, metadataKeyCodexImageGenerationModel) == "" {
			if model := MetadataString(srcMeta, metadataKeyCodexImageGenerationModel); model != "" {
				dstMeta[metadataKeyCodexImageGenerationModel] = model
			}
		}
	}

	if _, ok := MetadataBoolPresence(dstMeta, "using_api", "using-api", "usingApi"); !ok {
		if usingAPI, has := MetadataBoolPresence(srcMeta, "using_api", "using-api", "usingApi"); has {
			dstMeta["using_api"] = usingAPI
			dstAttrs["using_api"] = strconv.FormatBool(usingAPI)
		}
	}
}

func sameCredentialID(existing, incoming *coreauth.Auth) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if strings.TrimSpace(existing.ID) != "" && existing.ID == incoming.ID {
		return true
	}
	existingName := PublicFileName(existing)
	incomingName := PublicFileName(incoming)
	return existingName != "" && incomingName != "" && strings.EqualFold(existingName, incomingName)
}

func isAPIKeyCredential(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.TrimSpace(Attribute(auth, "api_key")) != "" {
		return true
	}
	accountType, _ := auth.AccountInfo()
	return strings.EqualFold(accountType, "api_key")
}

func isStableAccountSeedKind(seedKind string) bool {
	switch strings.TrimSpace(seedKind) {
	case "account_user_id", "account_id", "email":
		return true
	default:
		return false
	}
}

func samePhysicalOAuthAccount(incoming *coreauth.Auth, incomingIdentity *usage.AuthSubjectIdentity, existing *coreauth.Auth) bool {
	if incomingIdentity == nil || !isStableAccountSeedKind(incomingIdentity.SeedKind) {
		return false
	}
	existingIdentity := usage.ResolveAuthSubjectIdentity(existing)
	if existingIdentity == nil || !isStableAccountSeedKind(existingIdentity.SeedKind) {
		return false
	}
	if existingIdentity.ID == incomingIdentity.ID {
		return true
	}
	// Legacy Codex credentials written before chatgpt_user_id was persisted use
	// SeedKind="account_id", whereas new logins use "account_user_id". Match
	// them only when account_id and non-empty email both agree, so two distinct
	// members of the same Team workspace are never merged.
	if strings.EqualFold(incomingIdentity.Provider, "codex") {
		legacyIncoming := usage.LegacyAccountIDAuthSubjectID(incoming)
		legacyExisting := usage.LegacyAccountIDAuthSubjectID(existing)
		sameLegacy := (legacyIncoming != "" && legacyIncoming == existingIdentity.ID) ||
			(legacyExisting != "" && legacyExisting == incomingIdentity.ID)
		if sameLegacy &&
			incomingIdentity.Email != "" &&
			existingIdentity.Email != "" &&
			strings.EqualFold(incomingIdentity.Email, existingIdentity.Email) {
			return true
		}
	}
	return false
}

func sortReplacementSources(sources []*coreauth.Auth) {
	sort.Slice(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		aEnabled := !a.Disabled && a.Status != coreauth.StatusDisabled
		bEnabled := !b.Disabled && b.Status != coreauth.StatusDisabled
		if aEnabled != bEnabled {
			return aEnabled
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID < b.ID
	})
}

func inheritSubscriptionMetadata(dstMeta, srcMeta map[string]any) {
	if len(srcMeta) == 0 {
		return
	}
	_, hasDstStart := ExtractSubscriptionStartTimestamp(dstMeta)
	_, hasDstExpires := ExtractSubscriptionExpirationTimestamp(dstMeta)
	_, hasDstPeriod := ExtractSubscriptionPeriod(dstMeta)
	if hasDstStart || hasDstExpires || hasDstPeriod {
		return
	}
	if start, ok := ExtractSubscriptionStartTimestamp(srcMeta); ok {
		dstMeta["subscription_started_at"] = start.UTC().Format(time.RFC3339)
		if period, okPeriod := ExtractSubscriptionPeriod(srcMeta); okPeriod {
			dstMeta["subscription_period"] = period
		}
		return
	}
	if expires, ok := ExtractSubscriptionExpirationTimestamp(srcMeta); ok {
		dstMeta["subscription_expires_at"] = expires.UTC().Format(time.RFC3339)
		return
	}
	if period, okPeriod := ExtractSubscriptionPeriod(srcMeta); okPeriod {
		dstMeta["subscription_period"] = period
	}
}

func inheritStringSliceMetadata(dstMeta, srcMeta map[string]any, key string) {
	if len(srcMeta) == 0 {
		return
	}
	if _, exists := dstMeta[key]; exists {
		return
	}
	raw, exists := srcMeta[key]
	if !exists || raw == nil {
		return
	}
	values := stringSliceFromAny(raw)
	if values == nil {
		return
	}
	// Preserve an explicitly empty display_tags slice as well as non-empty lists.
	if len(values) == 0 && key != "display_tags" {
		return
	}
	dstMeta[key] = values
}

func stringSliceFromAny(raw any) []string {
	switch typed := raw.(type) {
	case []string:
		out := make([]string, len(typed))
		copy(out, typed)
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func metadataInt(meta map[string]any, keys ...string) (int, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	for _, key := range keys {
		raw, ok := meta[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return parsed, true
			}
		default:
			if parsed, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
