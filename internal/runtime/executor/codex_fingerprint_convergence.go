package executor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex device fingerprint convergence.
//
// A Codex client stamps every request with identifiers that upstream reads as
// "which machine" and "which conversation": installation id, session id, thread
// id and window id. When several people share one OAuth account through this
// proxy, each of their clients contributes its own set, so upstream sees a
// crowd of devices and sessions on a single account and applies device/session
// quota limits accordingly.
//
// Convergence rewrites those identifiers to values derived from the account
// itself, so the traffic reads as one installation instead of N. The four modes
// trade fidelity for aggressiveness; see config.CodexFingerprintConvergence*.
//
// Identifiers are derived (not random) so they survive restarts: the same
// account always produces the same installation and session id, which is what
// makes the device look persistent rather than freshly reinstalled per boot.

// codexConvergedIDs is the identifier set applied to one outbound request.
//
// Header rewriting and body rewriting must share a single instance: turn id is
// generated fresh per request, and a mismatch between the header copy and the
// client_metadata copy is itself a fingerprint.
type codexConvergedIDs struct {
	mode           string
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
	// clientRequestID mirrors x-client-request-id. Official Codex sets it to the
	// thread id, but device mode scopes whatever the client actually sent rather
	// than assuming that equality holds for every client.
	clientRequestID string
	// turnStartedAtUnixMs is stamped from the same clock that produced turnID
	// (a time-ordered UUIDv7). Keeping the client's original timestamp next to a
	// server-generated turn id would leave the two disagreeing about when the
	// turn started, which is a discrepancy a real client never produces.
	turnStartedAtUnixMs int64
	// scopedFromClient marks the device-mode snapshot, whose session, thread and
	// window ids are scoped rewrites of the client's own values instead of
	// proxy-invented ones. Projection then only touches fields the client sent,
	// and keeps its turn id and turn timestamp untouched.
	scopedFromClient bool
}

type codexConvergedIDsContextKeyType struct{}

var codexConvergedIDsContextKey codexConvergedIDsContextKeyType

// withCodexConvergedIDs stores a resolved identifier set on the context so the
// body rewrite in the executor and the header rewrite in applyCodexHeaders act
// on the same values.
func withCodexConvergedIDs(ctx context.Context, ids *codexConvergedIDs) context.Context {
	if ctx == nil || ids == nil {
		return ctx
	}
	return context.WithValue(ctx, codexConvergedIDsContextKey, ids)
}

func codexConvergedIDsFromContext(ctx context.Context) *codexConvergedIDs {
	if ctx == nil {
		return nil
	}
	ids, _ := ctx.Value(codexConvergedIDsContextKey).(*codexConvergedIDs)
	return ids
}

// codexConvergenceMode reports the effective convergence strength for this auth.
// Convergence rides on the Codex identity fingerprint switch: turning that off
// means "leave my outbound identity alone", which has to include device ids.
// When an auth file explicitly specifies a convergence mode in its metadata or attributes,
// that per-account override takes precedence over the global config.
func codexConvergenceMode(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if cfg == nil || !cfg.IdentityFingerprint.Codex.Enabled {
		return config.CodexFingerprintConvergenceOff
	}
	// API-key credentials are not OAuth accounts and carry no device quota, so
	// rewriting their identifiers would only add noise.
	if auth != nil && auth.Attributes != nil {
		if strings.TrimSpace(auth.Attributes["api_key"]) != "" {
			return config.CodexFingerprintConvergenceOff
		}
	}
	// Check per-account override first from attributes then metadata.
	if auth != nil {
		for _, key := range []string{"codex_convergence_mode", "convergence_mode", "codex-convergence-mode"} {
			if auth.Attributes != nil {
				if v, ok := auth.Attributes[key]; ok {
					v = strings.TrimSpace(strings.ToLower(v))
					if config.IsValidCodexFingerprintConvergenceMode(v) {
						return v
					}
				}
			}
			if auth.Metadata != nil {
				if raw, ok := auth.Metadata[key]; ok {
					if s, isStr := raw.(string); isStr {
						s = strings.TrimSpace(strings.ToLower(s))
						if config.IsValidCodexFingerprintConvergenceMode(s) {
							return s
						}
					}
				}
			}
		}
	}
	mode := strings.TrimSpace(strings.ToLower(cfg.IdentityFingerprint.Codex.ConvergenceMode))
	if mode == "" {
		mode = config.DefaultCodexFingerprintConvergenceMode
	}
	if !config.IsValidCodexFingerprintConvergenceMode(mode) {
		mode = config.DefaultCodexFingerprintConvergenceMode
	}
	return mode
}

// deriveStableUUIDv4 turns a seed into a fixed UUIDv4-shaped string. The same
// seed always yields the same value, which is what makes a derived installation
// id look like a real one that persists across restarts.
func deriveStableUUIDv4(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

// codexConvergenceScope returns the stable per-account seed component. It
// reuses the identity fingerprint account key so a credential keeps its device
// identity after moving between tenants, and falls back to the broader session
// isolation scope for auths that carry no resolvable subject.
func codexConvergenceScope(auth *cliproxyauth.Auth) string {
	if accountKey, _ := identityFingerprintAccount(auth); strings.TrimSpace(accountKey) != "" {
		return strings.TrimSpace(accountKey)
	}
	return sessionIsolationScope(auth)
}

// resolveConvergedInstallationID prefers an operator-pinned installation id so a
// value captured from a real Codex client can be replayed; otherwise it derives
// one from the account scope.
func resolveConvergedInstallationID(cfg *config.Config, scope string) string {
	if cfg != nil {
		if pinned := strings.TrimSpace(cfg.IdentityFingerprint.Codex.InstallationID); pinned != "" {
			return pinned
		}
	}
	if scope == "" {
		return ""
	}
	return deriveStableUUIDv4("clirelay:codex-install-id:v1:" + scope)
}

func resolveConvergedSessionID(scope string) string {
	if scope == "" {
		return ""
	}
	return deriveStableUUIDv4("clirelay:codex-session-id:v1:" + scope)
}

// resolveConvergedThreadID derives one thread per real client session, so a
// shared account still looks like a single user running several agents rather
// than a single endless conversation.
func resolveConvergedThreadID(scope, clientSessionID string) string {
	if scope == "" || clientSessionID == "" {
		return ""
	}
	return deriveStableUUIDv4("clirelay:codex-thread-id:v1:" + scope + ":" + clientSessionID)
}

// adoptScopedClientIdentity fills the snapshot with account-scoped rewrites of
// the identifiers the client itself sent, instead of proxy-invented ones.
//
// This is what device mode projects. Every field goes through the same scope,
// so the relationships the client established survive the rewrite: a root turn
// that arrived with session_id == thread_id still leaves with the two equal,
// and a window id keeps pointing at its own session. Fields the client did not
// send stay empty, and projection then leaves them alone.
func (ids *codexConvergedIDs) adoptScopedClientIdentity(scope string, clientHeaders http.Header) {
	if ids == nil || scope == "" {
		return
	}
	ids.scopedFromClient = true

	// Clients split these between real headers and the turn metadata blob; both
	// reach upstream, so both have to resolve to the same scoped value.
	metadata := ""
	if clientHeaders != nil {
		metadata = strings.TrimSpace(clientHeaders.Get("X-Codex-Turn-Metadata"))
	}
	if metadata != "" && !gjson.Valid(metadata) {
		metadata = ""
	}
	clientValue := func(metadataKey string, headerNames ...string) string {
		if clientHeaders != nil {
			for _, name := range headerNames {
				if v := strings.TrimSpace(clientHeaders.Get(name)); v != "" {
					return v
				}
			}
		}
		if metadata != "" && metadataKey != "" {
			if v := strings.TrimSpace(gjson.Get(metadata, metadataKey).String()); v != "" {
				return v
			}
		}
		return ""
	}

	ids.sessionID = codexScopedIdentifier(scope, clientValue("session_id", "Session-Id", "Session_id"))
	ids.threadID = codexScopedIdentifier(scope, clientValue("thread_id", "Thread-Id"))
	ids.windowID = codexScopedIdentifier(scope, clientValue("window_id", "X-Codex-Window-Id"))
	ids.clientRequestID = codexScopedIdentifier(scope, clientValue("", "X-Client-Request-Id"))
}

// extractClientSessionID reads the client's own session identifier before any
// rewrite. Codex CLI sends the hyphenated form; the underscore form is accepted
// as a fallback because some clients and earlier proxy layers emit it.
func extractClientSessionID(h http.Header) string {
	if h == nil {
		return ""
	}
	for _, key := range []string{"session-id", "session_id"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// resolveCodexConvergedIDs computes the identifier set for one request.
// It returns nil when convergence is off or no stable account scope exists,
// in which case callers must leave the client identifiers untouched.
//
// The result carries a freshly generated turn id, so callers must resolve once
// per request and share the result between header and body rewriting.
func resolveCodexConvergedIDs(cfg *config.Config, auth *cliproxyauth.Auth, clientHeaders http.Header) *codexConvergedIDs {
	mode := codexConvergenceMode(cfg, auth)
	if mode == config.CodexFingerprintConvergenceOff {
		return nil
	}
	scope := codexConvergenceScope(auth)
	installationID := resolveConvergedInstallationID(cfg, scope)
	if installationID == "" {
		return nil
	}

	ids := &codexConvergedIDs{mode: mode, installationID: installationID}
	if mode == config.CodexFingerprintConvergenceDevice {
		// Deliberately the session isolation scope, not the convergence scope
		// above: the cache helper already maps prompt_cache_key through that one,
		// and the two must agree or the body cache key and the wire session end up
		// naming different sessions. Installation keeps the convergence scope so
		// existing device identities do not shift.
		ids.adoptScopedClientIdentity(sessionIsolationScope(auth), clientHeaders)
		return ids
	}

	ids.sessionID = resolveConvergedSessionID(scope)
	if ids.sessionID == "" {
		// Without a stable session id the session/full modes cannot converge
		// anything beyond the device; degrade instead of emitting empty headers.
		ids.mode = config.CodexFingerprintConvergenceDevice
		return ids
	}
	switch mode {
	case config.CodexFingerprintConvergenceSession:
		ids.threadID = resolveConvergedThreadID(scope, extractClientSessionID(clientHeaders))
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
	case config.CodexFingerprintConvergenceFull:
		ids.threadID = ids.sessionID
	}
	turnStartedAt := time.Now()
	ids.turnID = newCodexTurnID()
	ids.turnStartedAtUnixMs = turnStartedAt.UnixMilli()
	ids.windowID = ids.threadID + ":0"
	return ids
}

// prepareCodexConvergence resolves the identifier set once per request and
// returns a context carrying it. Header rewriting in applyCodexHeaders reads it
// back from there, so the headers and the request body agree on the per-request
// turn id instead of each generating its own.
func (e *CodexExecutor) prepareCodexConvergence(ctx context.Context, auth *cliproxyauth.Auth) (context.Context, *codexConvergedIDs) {
	ids := resolveCodexConvergedIDs(e.cfg, auth, identityFingerprintHeadersFromContext(ctx))
	if ids == nil {
		return ctx, nil
	}
	return withCodexConvergedIDs(ctx, ids), ids
}

// newCodexTurnID mirrors the time-ordered turn identifier a real client emits.
func newCodexTurnID() string {
	if v, err := uuid.NewV7(); err == nil {
		return v.String()
	}
	return uuid.NewString()
}

// applyCodexConvergenceHeaders rewrites the device identifiers on the outbound
// request. It must run after client headers have been copied in, so the values
// it writes are the ones that survive to the wire.
//
// clientHeaders is the original inbound request. Identifiers this proxy does not
// otherwise forward are only emitted when the client sent them: this layer
// converges what upstream would have seen anyway and must not introduce a header
// upstream was never going to receive, which would add fingerprint surface
// rather than remove it. The turn metadata payload is exempt because
// applyCodexHeaders forwards it verbatim, so its embedded identifiers do reach
// upstream and are the main leak this feature closes.
func applyCodexConvergenceHeaders(h http.Header, ids *codexConvergedIDs, clientHeaders http.Header) {
	if h == nil || ids == nil || ids.installationID == "" {
		return
	}

	setIfClientSent(h, clientHeaders, "X-Codex-Installation-Id", ids.installationID)

	if ids.mode == config.CodexFingerprintConvergenceDevice {
		if ids.scopedFromClient {
			// The scoped ids replace the client's own values wherever they travel.
			// Session_id and Conversation_id are already on the request: the cache
			// helper derived them from prompt_cache_key, which for a default Codex
			// client equals the session id and therefore scopes to the same value.
			// Pinning them here makes that hold even when a client overrides the
			// cache key, so header and body never describe two different sessions.
			setIfClientSent(h, clientHeaders, "Session-Id", ids.sessionID)
			setIfClientSent(h, clientHeaders, "Session_id", ids.sessionID)
			setIfClientSent(h, clientHeaders, "Conversation_id", ids.sessionID)
			setIfClientSent(h, clientHeaders, "Thread-Id", ids.threadID)
			setIfClientSent(h, clientHeaders, "X-Codex-Window-Id", ids.windowID)
			setIfClientSent(h, clientHeaders, "X-Client-Request-Id", ids.clientRequestID)
		}
		rewriteCodexTurnMetadataHeader(h, ids.turnMetadataFields())
		return
	}

	setIfClientSent(h, clientHeaders, "X-Codex-Window-Id", ids.windowID)
	setIfClientSent(h, clientHeaders, "Thread-Id", ids.threadID)
	// x-client-request-id is forwarded verbatim by applyCodexHeaders, so it
	// reaches upstream and must be converged whether or not the client set it.
	h.Set("X-Client-Request-Id", ids.threadID)
	// Session_id always goes out: without convergence the per-request session
	// mode stamps a fresh random id on every call, which makes one account look
	// like an endless stream of new sessions. Both spellings are pinned so a
	// client that used the other form cannot re-expose its original value.
	h.Set("Session-Id", ids.sessionID)
	h.Set("Session_id", ids.sessionID)

	rewriteCodexTurnMetadataHeader(h, ids.turnMetadataFields())
}

// setIfClientSent writes value only when the inbound request carried that
// header, so convergence never widens what upstream can observe.
func setIfClientSent(h, clientHeaders http.Header, key, value string) {
	if value == "" {
		return
	}
	if strings.TrimSpace(h.Get(key)) == "" {
		if clientHeaders == nil || strings.TrimSpace(clientHeaders.Get(key)) == "" {
			return
		}
	}
	h.Set(key, value)
}

// deviceOnly narrows an identifier set to device-level convergence.
//
// The websocket path uses this: there Session_id is deliberately pinned to
// Conversation_id and prompt_cache_key so upstream can match the prompt cache,
// and rewriting the session or thread would break that three-way association for
// a saving that only matters on the HTTP path. Converging the installation is
// safe because nothing else is keyed off it.
func (ids *codexConvergedIDs) deviceOnly() *codexConvergedIDs {
	if ids == nil {
		return nil
	}
	narrowed := *ids
	narrowed.mode = config.CodexFingerprintConvergenceDevice
	return &narrowed
}

// turnMetadataFields returns the turn-metadata entries this mode converges.
// Device mode only claims the installation, so it must not touch the session,
// thread or turn fields the client sent.
func (ids *codexConvergedIDs) turnMetadataFields() map[string]any {
	fields := map[string]any{"installation_id": ids.installationID}
	if ids.scopedFromClient {
		// Device mode rewrites exactly the ids it scoped. turn_id and
		// turn_started_at_unix_ms stay as the client sent them: they identify a
		// single turn and carry no cross-request identity, so rewriting them
		// would add divergence without hiding anything.
		for key, value := range map[string]string{
			"session_id": ids.sessionID,
			"thread_id":  ids.threadID,
			"window_id":  ids.windowID,
		} {
			if value != "" {
				fields[key] = value
			}
		}
		return fields
	}
	if ids.mode == config.CodexFingerprintConvergenceDevice {
		return fields
	}
	fields["session_id"] = ids.sessionID
	fields["thread_id"] = ids.threadID
	fields["turn_id"] = ids.turnID
	fields["window_id"] = ids.windowID
	fields["turn_started_at_unix_ms"] = ids.turnStartedAtUnixMs
	return fields
}

// rewriteCodexTurnMetadataHeader replaces the given fields inside the
// x-codex-turn-metadata JSON payload while preserving every other field the
// client sent (sandbox, thread_source and similar), because dropping them would
// itself change the fingerprint.
func rewriteCodexTurnMetadataHeader(h http.Header, fields map[string]any) {
	raw := strings.TrimSpace(h.Get("X-Codex-Turn-Metadata"))
	rebuilt, ok := rewriteCodexTurnMetadataJSON(raw, fields)
	if !ok {
		return
	}
	h.Set("X-Codex-Turn-Metadata", rebuilt)
}

// rewriteCodexTurnMetadataJSON applies fields to a turn-metadata JSON document.
// It reports false when the payload is absent or not parseable, so callers leave
// the original value untouched rather than replacing it with a partial rewrite.
//
// Only keys the client already sent are replaced. Convergence exists to shrink
// what upstream can distinguish, so adding a field the client omitted would work
// against the goal even when the field is one a real client usually sends.
func rewriteCodexTurnMetadataJSON(raw string, fields map[string]any) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) {
		return "", false
	}
	rebuilt := raw
	for _, key := range sortedCodexMetadataKeys(fields) {
		if !gjson.Get(rebuilt, key).Exists() {
			continue
		}
		next, err := sjson.Set(rebuilt, key, fields[key])
		if err != nil {
			return "", false
		}
		rebuilt = next
	}
	return rebuilt, true
}

// applyCodexConvergenceClientMetadata rewrites the identifiers duplicated in the
// request body. Fields are only replaced when the client already sent
// client_metadata: fabricating the object for clients that never send it would
// add a fingerprint rather than converge one.
func applyCodexConvergenceClientMetadata(body []byte, ids *codexConvergedIDs) []byte {
	if len(body) == 0 || ids == nil || ids.installationID == "" {
		return body
	}
	if !gjson.GetBytes(body, "client_metadata").IsObject() {
		return body
	}

	fields := map[string]any{
		"client_metadata.x-codex-installation-id": ids.installationID,
	}
	switch {
	case ids.scopedFromClient:
		// Same snapshot as the headers, so the body cannot describe a different
		// session than the wire does.
		for key, value := range map[string]string{
			"client_metadata.session_id":        ids.sessionID,
			"client_metadata.thread_id":         ids.threadID,
			"client_metadata.x-codex-window-id": ids.windowID,
		} {
			if value != "" {
				fields[key] = value
			}
		}
	case ids.mode != config.CodexFingerprintConvergenceDevice:
		fields["client_metadata.session_id"] = ids.sessionID
		fields["client_metadata.thread_id"] = ids.threadID
		fields["client_metadata.turn_id"] = ids.turnID
		fields["client_metadata.x-codex-window-id"] = ids.windowID
	}

	for _, key := range sortedCodexMetadataKeys(fields) {
		// Only rewrite identifiers the client actually sent: adding a field it
		// never included would widen the fingerprint instead of converging it.
		if !gjson.GetBytes(body, key).Exists() {
			continue
		}
		next, err := sjson.SetBytes(body, key, fields[key])
		if err != nil {
			return body
		}
		body = next
	}
	return rewriteCodexClientMetadataEmbeddedTurnMetadata(body, ids)
}

// rewriteCodexClientMetadataEmbeddedTurnMetadata patches the turn metadata that
// clients embed inside client_metadata as a JSON string.
func rewriteCodexClientMetadataEmbeddedTurnMetadata(body []byte, ids *codexConvergedIDs) []byte {
	raw := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	rebuilt, ok := rewriteCodexTurnMetadataJSON(raw, ids.turnMetadataFields())
	if !ok {
		return body
	}
	next, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", rebuilt)
	if err != nil {
		return body
	}
	return next
}

// sortedCodexMetadataKeys keeps rewrite order deterministic so the resulting
// JSON field order does not vary between otherwise identical requests.
func sortedCodexMetadataKeys(fields map[string]any) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
