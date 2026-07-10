package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	codexServerSessionOnce sync.Once
	codexServerSessionID   string
)

const codexIdentityFingerprintSelectionCacheTTL = 30 * time.Second

var (
	runtimeListCodexIdentityFingerprintProfiles     = usage.ListIdentityFingerprintProfiles
	runtimeGetCodexIdentityFingerprintAccountPolicy = usage.GetIdentityFingerprintAccountPolicy
)

func callRuntimeListCodexIdentityFingerprintProfiles(provider identityfingerprint.Provider, accountKey string) ([]identityfingerprint.LearnedRecord, error) {
	runtimeIdentityFingerprintStoreFuncMu.RLock()
	fn := runtimeListCodexIdentityFingerprintProfiles
	runtimeIdentityFingerprintStoreFuncMu.RUnlock()
	return fn(provider, accountKey)
}

func callRuntimeGetCodexIdentityFingerprintAccountPolicy(provider identityfingerprint.Provider, accountKey string) (identityfingerprint.AccountPolicy, error) {
	runtimeIdentityFingerprintStoreFuncMu.RLock()
	fn := runtimeGetCodexIdentityFingerprintAccountPolicy
	runtimeIdentityFingerprintStoreFuncMu.RUnlock()
	return fn(provider, accountKey)
}

type codexIdentityFingerprintSelectionEntry struct {
	selection identityfingerprint.ProfileSelection
	expiresAt time.Time
}

var codexIdentityFingerprintSelectionCache = struct {
	sync.Mutex
	entries    map[string]codexIdentityFingerprintSelectionEntry
	refreshing map[string]struct{}
}{
	entries:    map[string]codexIdentityFingerprintSelectionEntry{},
	refreshing: map[string]struct{}{},
}

func init() {
	usage.RegisterIdentityFingerprintInvalidationHook(func(provider identityfingerprint.Provider, accountKey string) {
		if provider != identityfingerprint.ProviderCodex {
			return
		}
		invalidateCachedCodexIdentityFingerprintSelection(accountKey)
	})
}

func codexServerStableSessionID() string {
	codexServerSessionOnce.Do(func() {
		codexServerSessionID = uuid.NewString()
	})
	return codexServerSessionID
}

func codexIdentityFingerprint(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (config.CodexIdentityFingerprintConfig, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.Codex.Enabled {
		return config.CodexIdentityFingerprintConfig{}, false
	}
	if !isCodexOAuthAdmissionAuth(auth) {
		resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
		return resolved, true
	}

	// Learning happens before selection so the first trusted request for a new
	// client variant can immediately use its own complete identity bundle.
	observed := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderCodex, auth, ctx)
	accountKey, _ := identityFingerprintAccount(auth)
	if selection, ok := getCachedCodexIdentityFingerprintSelection(accountKey); ok {
		if selection.Profile != nil {
			resolved, _ := identityfingerprint.ResolveCodexProfile(cfg.IdentityFingerprint.Codex, selection.Profile)
			return resolved, true
		}
		resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
		return resolved, true
	}
	scheduleCodexIdentityFingerprintSelectionRefresh(accountKey)
	selection := codexIdentityFingerprintSelectionFromRuntimeCache(accountKey, observed)
	setCachedCodexIdentityFingerprintSelection(accountKey, selection)
	if selection.Profile != nil {
		resolved, _ := identityfingerprint.ResolveCodexProfile(cfg.IdentityFingerprint.Codex, selection.Profile)
		return resolved, true
	}
	resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
	return resolved, true
}

func getCachedCodexIdentityFingerprintSelection(accountKey string) (identityfingerprint.ProfileSelection, bool) {
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return identityfingerprint.ProfileSelection{}, false
	}
	now := time.Now()
	codexIdentityFingerprintSelectionCache.Lock()
	entry, ok := codexIdentityFingerprintSelectionCache.entries[accountKey]
	if !ok {
		codexIdentityFingerprintSelectionCache.Unlock()
		return identityfingerprint.ProfileSelection{}, false
	}
	stale := now.After(entry.expiresAt)
	selection := cloneCodexIdentityFingerprintSelection(entry.selection)
	codexIdentityFingerprintSelectionCache.Unlock()
	if stale {
		scheduleCodexIdentityFingerprintSelectionRefresh(accountKey)
	}
	return selection, true
}

func setCachedCodexIdentityFingerprintSelection(accountKey string, selection identityfingerprint.ProfileSelection) {
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return
	}
	codexIdentityFingerprintSelectionCache.Lock()
	codexIdentityFingerprintSelectionCache.entries[accountKey] = codexIdentityFingerprintSelectionEntry{
		selection: cloneCodexIdentityFingerprintSelection(selection),
		expiresAt: time.Now().Add(codexIdentityFingerprintSelectionCacheTTL),
	}
	codexIdentityFingerprintSelectionCache.Unlock()
}

func invalidateCachedCodexIdentityFingerprintSelection(accountKey string) {
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return
	}
	now := time.Now().Add(-time.Second)
	codexIdentityFingerprintSelectionCache.Lock()
	if entry, ok := codexIdentityFingerprintSelectionCache.entries[accountKey]; ok {
		entry.expiresAt = now
		codexIdentityFingerprintSelectionCache.entries[accountKey] = entry
	}
	codexIdentityFingerprintSelectionCache.Unlock()
	scheduleCodexIdentityFingerprintSelectionRefresh(accountKey)
}

func scheduleCodexIdentityFingerprintSelectionRefresh(accountKey string) {
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return
	}
	codexIdentityFingerprintSelectionCache.Lock()
	if _, ok := codexIdentityFingerprintSelectionCache.refreshing[accountKey]; ok {
		codexIdentityFingerprintSelectionCache.Unlock()
		return
	}
	codexIdentityFingerprintSelectionCache.refreshing[accountKey] = struct{}{}
	codexIdentityFingerprintSelectionCache.Unlock()

	go func() {
		defer func() {
			codexIdentityFingerprintSelectionCache.Lock()
			delete(codexIdentityFingerprintSelectionCache.refreshing, accountKey)
			codexIdentityFingerprintSelectionCache.Unlock()
		}()
		profiles, err := callRuntimeListCodexIdentityFingerprintProfiles(identityfingerprint.ProviderCodex, accountKey)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: list Codex profiles")
			return
		}
		policy, err := callRuntimeGetCodexIdentityFingerprintAccountPolicy(identityfingerprint.ProviderCodex, accountKey)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: load Codex account policy")
		}
		selection := identityfingerprint.SelectCodexProfile(profiles, policy)
		setCachedCodexIdentityFingerprintSelection(accountKey, selection)
	}()
}

func codexIdentityFingerprintSelectionFromRuntimeCache(accountKey string, observed *identityfingerprint.LearnedRecord) identityfingerprint.ProfileSelection {
	profiles := listCachedRuntimeIdentityFingerprintProfiles(identityfingerprint.ProviderCodex, accountKey)
	if observed != nil {
		profiles = upsertRuntimeCodexProfileSnapshot(profiles, observed)
	}
	policy := identityfingerprint.NormalizeAccountPolicy(identityfingerprint.ProviderCodex, accountKey, identityfingerprint.AccountPolicy{})
	return identityfingerprint.SelectCodexProfile(profiles, policy)
}

func upsertRuntimeCodexProfileSnapshot(profiles []identityfingerprint.LearnedRecord, observed *identityfingerprint.LearnedRecord) []identityfingerprint.LearnedRecord {
	if observed == nil || strings.TrimSpace(observed.ProfileKey) == "" {
		return profiles
	}
	for i := range profiles {
		if profiles[i].ProfileKey == observed.ProfileKey {
			profiles[i] = *cloneRuntimeIdentityFingerprintRecord(observed)
			return profiles
		}
	}
	return append(profiles, *cloneRuntimeIdentityFingerprintRecord(observed))
}

func cloneCodexIdentityFingerprintSelection(selection identityfingerprint.ProfileSelection) identityfingerprint.ProfileSelection {
	selection.Profile = cloneRuntimeIdentityFingerprintRecord(selection.Profile)
	return selection
}

func codexFingerprintSessionID(fp config.CodexIdentityFingerprintConfig) string {
	switch strings.TrimSpace(strings.ToLower(fp.SessionMode)) {
	case "fixed":
		if strings.TrimSpace(fp.SessionID) != "" {
			return strings.TrimSpace(fp.SessionID)
		}
		return codexServerStableSessionID()
	case "per-request":
		return uuid.NewString()
	default:
		return codexServerStableSessionID()
	}
}

func applyCodexIdentityFingerprintHeaders(headers http.Header, fp config.CodexIdentityFingerprintConfig, websocket bool) {
	if headers == nil {
		return
	}
	// Product identity headers are an atomic bundle. Clear any values copied
	// from the inbound request or an earlier layer before applying the selected
	// profile so an absent field cannot leak in from another identity.
	for _, key := range []string{"Version", "User-Agent", "OpenAI-Beta", "X-Codex-Beta-Features"} {
		headers.Del(key)
	}
	// Follow upstream codex-tui behavior: only send headers when values are non-empty.
	if strings.TrimSpace(fp.Version) != "" {
		headers.Set("Version", fp.Version)
	}
	if strings.TrimSpace(fp.UserAgent) != "" {
		headers.Set("User-Agent", fp.UserAgent)
	}
	if strings.TrimSpace(headers.Get("Session_id")) == "" {
		headers.Set("Session_id", codexFingerprintSessionID(fp))
	}
	if websocket {
		if strings.TrimSpace(fp.WebsocketBeta) != "" {
			headers.Set("OpenAI-Beta", fp.WebsocketBeta)
		}
	}
	if strings.TrimSpace(fp.BetaFeatures) != "" {
		headers.Set("X-Codex-Beta-Features", fp.BetaFeatures)
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || isCodexFingerprintRuntimeBlockedHeader(key) {
			continue
		}
		headers.Set(key, value)
	}
}

func isCodexFingerprintRuntimeBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection", "chatgpt-account-id",
		"user-agent", "version", "session_id", "session-id", "originator", "openai-beta", "x-codex-beta-features":
		return true
	default:
		return false
	}
}
