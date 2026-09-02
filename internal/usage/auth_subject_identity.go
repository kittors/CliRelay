package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// AuthSubjectIdentity is the single account identity contract used by status,
// usage, quota and identity fingerprints. AccountKey must always equal ID.
//
// A subject is meant to be the physical upstream account: one account, one
// subject, bound by however many tenants happen to hold a credential for it.
// Sharing therefore follows whether the seed identifies that account —
// the provider's account id, the account's email, or the credential's own
// provider-scoped id all do. Only auth_index, which is local bookkeeping and
// names no account at all, stays tenant scoped.
type AuthSubjectIdentity struct {
	ID            string
	Provider      string
	AccountID     string
	UserID        string
	Email         string
	SeedKind      string
	SeedHash      string
	SubjectScope  string
	ShareEligible bool
	ShareReason   string
}

type AuthSubjectMatcher struct {
	SubjectID      string
	AuthIndexes    []string
	SourceAliases  []string
	ChannelAliases []string
}

func ResolveAuthSubjectIdentity(auth *coreauth.Auth) *AuthSubjectIdentity {
	if auth == nil {
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		provider = "unknown"
	}
	tenantID := coreauth.NormalizedTenantID(auth.TenantID)
	accountID := authMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id")
	userID := authMetadataString(auth.Metadata, "chatgpt_user_id", "chatgptUserId")
	email := strings.ToLower(authEmail(auth))

	seedKind := ""
	seedValue := ""
	shareEligible := false
	subjectScope := AIAccountSubjectScopeTenant
	shareReason := "fallback_identity_is_tenant_scoped"
	switch {
	case provider == "codex" && accountID != "" && userID != "":
		seedKind = "account_user_id"
		seedValue = accountID + "\x1f" + userID
		shareEligible = true
		subjectScope = AIAccountSubjectScopeShared
		shareReason = "stable_provider_account_and_user_id"
	case accountID != "":
		seedKind = "account_id"
		seedValue = accountID
		shareEligible = true
		shareReason = "stable_provider_account_id"
	case email != "":
		// The email is the account, not a property of the tenant that mounted it.
		// Folding tenant_id into this seed split one Google account across every
		// tenant holding a credential for it, so each tenant saw a fraction of the
		// account's usage next to the whole account's quota.
		seedKind = "email"
		seedValue = email
		shareEligible = true
		shareReason = "provider_account_email"
	case providerScopedAuthID(auth) != "":
		// API-key credentials land here: their id is "<provider>:apikey:<digest>",
		// which is the same string wherever the key is mounted once the tenant
		// prefix — the only tenant-local part — is removed.
		seedKind = "auth_id"
		seedValue = providerScopedAuthID(auth)
		shareEligible = true
		shareReason = "provider_scoped_auth_id"
	default:
		authIndex := strings.TrimSpace(auth.EnsureIndex())
		if authIndex == "" {
			return nil
		}
		// auth_index is local bookkeeping and identifies no account, so it cannot
		// be trusted to mean "the same account" across tenants.
		seedKind = "auth_index"
		seedValue = authIndex
	}
	if shareEligible {
		subjectScope = AIAccountSubjectScopeShared
	}

	subjectSeed := []string{provider, seedKind, seedValue}
	seedHashValue := seedValue
	if !shareEligible {
		subjectSeed = []string{provider, seedKind, tenantID, seedValue}
		seedHashValue = tenantID + "\x1f" + seedValue
	}

	return &AuthSubjectIdentity{
		ID:            stableAuthSubjectID(subjectSeed...),
		Provider:      provider,
		AccountID:     accountID,
		UserID:        userID,
		Email:         email,
		SeedKind:      seedKind,
		SeedHash:      stableSeedHash(seedHashValue),
		SubjectScope:  subjectScope,
		ShareEligible: shareEligible,
		ShareReason:   shareReason,
	}
}

// providerScopedAuthID strips the tenant prefix an auth id is stored with.
//
// Ids are persisted as "<tenant-uuid>/<provider>:apikey:<digest>" (the system
// tenant keeps the bare form). Everything after the prefix is derived from the
// credential itself, so removing it yields the same string for one credential no
// matter which tenant mounted it — while two different keys still differ, their
// digests being taken from the key material.
func providerScopedAuthID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return ""
	}
	slash := strings.Index(id, "/")
	if slash <= 0 {
		return id
	}
	if _, err := uuid.Parse(id[:slash]); err != nil {
		return id
	}
	return strings.TrimSpace(id[slash+1:])
}

// LegacyTenantScopedAuthSubjectID recomputes the subject id this auth had while
// every non-account_id seed was tenant scoped. The migration needs it to find
// the rows an account accumulated under its old, split identity; nothing on the
// live path may use it.
func LegacyTenantScopedAuthSubjectID(auth *coreauth.Auth) string {
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		return ""
	}
	// auth_index never changed shape. account_id did — Codex now prefers
	// account_user_id — but that predecessor is LegacyAccountIDAuthSubjectID,
	// not a tenant-scoped id.
	seedValue := ""
	switch identity.SeedKind {
	case "email":
		seedValue = identity.Email
	case "auth_id":
		// The legacy seed carried the id exactly as stored, prefix included.
		seedValue = strings.TrimSpace(auth.ID)
	default:
		return ""
	}
	if seedValue == "" {
		return ""
	}
	return stableAuthSubjectID(
		identity.Provider,
		identity.SeedKind,
		coreauth.NormalizedTenantID(auth.TenantID),
		seedValue,
	)
}

// LegacyAccountIDAuthSubjectID is the subject this Codex credential used before
// the seed became account_id+user_id. Personal Pro accounts kept their usage
// there when the Team-collision fix started hashing the user id too, so the
// card next to a still-correct WHAM bar read a few hours of traffic as "this
// week". API keys and account-id-only auths have no such predecessor.
func LegacyAccountIDAuthSubjectID(auth *coreauth.Auth) string {
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.SeedKind != "account_user_id" {
		return ""
	}
	accountID := strings.TrimSpace(identity.AccountID)
	if accountID == "" {
		return ""
	}
	return stableAuthSubjectID(identity.Provider, "account_id", accountID)
}

func BuildAuthSubjectMatcher(current *coreauth.Auth, auths []*coreauth.Auth) AuthSubjectMatcher {
	var matcher AuthSubjectMatcher
	if current == nil {
		return matcher
	}

	baseIdentity := ResolveAuthSubjectIdentity(current)
	if baseIdentity != nil {
		matcher.SubjectID = strings.TrimSpace(baseIdentity.ID)
	}

	addAuthIndex := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range matcher.AuthIndexes {
			if existing == value {
				return
			}
		}
		matcher.AuthIndexes = append(matcher.AuthIndexes, value)
	}
	addAlias := func(target *[]string, value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range *target {
			if existing == value {
				return
			}
		}
		*target = append(*target, value)
	}

	addAuthIndex(current.EnsureIndex())
	if email := authEmail(current); email != "" {
		addAlias(&matcher.SourceAliases, email)
		addAlias(&matcher.ChannelAliases, email)
	}
	if _, accountInfo := current.AccountInfo(); accountInfo != "" {
		addAlias(&matcher.SourceAliases, accountInfo)
	}
	if channelName := current.ChannelName(); channelName != "" {
		addAlias(&matcher.ChannelAliases, channelName)
	}

	if baseIdentity == nil || baseIdentity.ID == "" {
		return matcher
	}

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		identity := ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID != baseIdentity.ID {
			continue
		}
		addAuthIndex(auth.EnsureIndex())
		if email := authEmail(auth); email != "" {
			addAlias(&matcher.SourceAliases, email)
			addAlias(&matcher.ChannelAliases, email)
		}
		if _, accountInfo := auth.AccountInfo(); accountInfo != "" {
			addAlias(&matcher.SourceAliases, accountInfo)
		}
		if channelName := auth.ChannelName(); channelName != "" {
			addAlias(&matcher.ChannelAliases, channelName)
		}
	}

	return matcher
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if email := authMetadataString(auth.Metadata, "email"); email != "" {
		return email
	}
	if auth.Attributes != nil {
		for _, key := range []string{"email", "account_email"} {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func authMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if raw, ok := metadata[key].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

func stableSeedHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func stableAuthSubjectID(parts ...string) string {
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}
	if len(normalized) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(normalized, "\x1f")))
	return "authsub_" + hex.EncodeToString(sum[:8])
}
