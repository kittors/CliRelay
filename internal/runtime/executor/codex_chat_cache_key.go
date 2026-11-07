package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Prompt cache key for Chat Completions callers.
//
// Upstream routes requests that share a prompt_cache_key to one cache node. A
// Responses or Claude client already gives this proxy something to key on; a
// Chat Completions client gives nothing, and the request used to leave without a
// key. Worse, with the identity fingerprint on (the default) the Session_id
// header that rides with it is a fresh random UUID per request, so the one other
// signal upstream could route on changed every turn too. A conversation that
// resends a growing prefix therefore kept landing on cold nodes.
//
// The key is derived, not minted: a hash of everything that must keep two
// conversations apart, so it is the same on every turn, on every node and across
// restarts without any shared state.
//
//   - the upstream account: one conversation never presents the same key through
//     two accounts, which would link them;
//   - the tenant and the downstream API key: two callers never share a cache
//     domain, even when they send byte-identical prompts. Tenant isolation is
//     the reason this is a hash of the caller and not of the prompt alone;
//   - the model, without any reasoning-level suffix, so changing effort in the
//     middle of a conversation does not move it;
//   - the conversation: the session identity routing already uses for
//     stickiness (session metadata and fields, the client's own prompt_cache_key,
//     falling back to the first user message), so the key and the account the
//     conversation is pinned to cannot disagree.
//
// Nothing readable survives the hash: the API key and the conversation text only
// ever enter it through SHA-256, and what leaves is a UUID-shaped value, the form
// of identifier a real Codex client sends.
func codexChatPromptCacheKey(ctx context.Context, auth *cliproxyauth.Auth, model string, opts cliproxyexecutor.Options, payload []byte) string {
	account := sessionIsolationScope(auth)
	if account == "" {
		// Without an account identity the key cannot be kept apart per account.
		return ""
	}
	conversation := codexSessionPromptCacheSeed(opts, payload)
	if conversation == "" {
		return ""
	}
	tenant := cliproxyauth.NormalizedTenantID(metadataText(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	return deriveStableUUIDv4(strings.Join([]string{
		"clirelay:codex-chat-cache-key:v1",
		account,
		thinking.ParseSuffix(model).ModelName,
		tenant,
		codexDownstreamKeyDigest(ctx),
		conversation,
	}, "\x00"))
}

// codexDownstreamKeyDigest identifies the caller's API key without carrying it.
func codexDownstreamKeyDigest(ctx context.Context) string {
	key := apiKeyFromContext(ctx)
	if key == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// codexChatSessionHeaderApplies reports whether a Chat request's derived key may
// also be sent as the Session_id header.
//
// The header is only ever a replacement for a value that would go out anyway:
// with the identity fingerprint enabled and its session mode per-request, every
// request already carries a random Session_id, and making it the conversation's
// stable value adds no header and no field to what upstream sees. Everywhere else
// this proxy has chosen what the header says (a fixed or server-stable session,
// or the fingerprint switched off to leave the outbound identity alone) and that
// choice wins. Conversation_id and Thread-Id are not sent at all: a Chat client
// sends neither, and convergence only rewrites identifiers the client sent.
func codexChatSessionHeaderApplies(cfg *config.Config) bool {
	if cfg == nil || !cfg.IdentityFingerprint.Codex.Enabled {
		return false
	}
	mode := config.CleanCodexIdentityFingerprint(cfg.IdentityFingerprint.Codex).SessionMode
	return mode == "" || mode == config.DefaultCodexFingerprintSessionMode
}
