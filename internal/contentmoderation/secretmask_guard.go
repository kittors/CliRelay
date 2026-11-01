package contentmoderation

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// GuardRequest implements coreauth.RequestGuard: the word guard (guardWords)
// and then, for a request it lets through, the tenant's secret masking
// (maskSecrets), on the request as the word guard left it.
func (m *RuntimeModerator) GuardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) coreauth.RequestGuardResult {
	result := m.guardWords(ctx, req, opts)
	if result.Blocked {
		return result
	}
	return m.maskSecrets(ctx, req, opts, result)
}

func (m *RuntimeModerator) secretMaskingRegistry() *SecretMaskingRegistry {
	if m != nil && m.secretMasking != nil {
		return m.secretMasking
	}
	return SecretMasking()
}

// maskSecrets replaces the secrets of a request with placeholders when its
// tenant has masking on, and adds the reply filter that puts them back. result
// is what the word guard made of the request; the bodies it rewrote are the
// ones masked.
//
// A request whose secrets cannot all be replaced is not sent: the tenant asked
// for its secrets to stay here.
func (m *RuntimeModerator) maskSecrets(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, result coreauth.RequestGuardResult) coreauth.RequestGuardResult {
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	settings := m.secretMaskingRegistry().For(tenantID)
	if !settings.Enabled {
		return result
	}
	key, ok := settings.placeholderKey()
	if !ok {
		// Never saved so: switching masking on makes the key. A row edited by
		// hand does not get the request sent unmasked.
		m.warnSecretMasking(tenantID, "secret masking is on but the tenant's key is unusable; requests are refused until it is switched off and on again")
		return coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusServiceUnavailable,
			Message: "Secret masking is on for this tenant but is not set up correctly, so the request was not sent. Ask an administrator to switch it off and on again."}
	}

	original, payload := opts.OriginalRequest, req.Payload
	if result.OriginalRequest != nil {
		original = result.OriginalRequest
	}
	if result.Payload != nil {
		payload = result.Payload
	}
	table := newSecretTable(key)
	format := opts.SourceFormat
	refuse := func(failure string) coreauth.RequestGuardResult {
		setSecretMaskingSnapshot(ctx, &SecretMaskingSnapshot{Refused: failure})
		m.warnSecretMasking(tenantID, "secret masking refused a request: "+failure)
		return coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusBadRequest,
			Message: "The request holds a secret that secret masking cannot replace (" + failure + "), so it was not sent. Remove the secret, or ask an administrator to switch secret masking off."}
	}
	if len(original) == 0 || sameStorage(original, payload) {
		// One body: both names keep pointing at one slice, which is how the
		// executors know not to translate it twice.
		masked, _, failure := maskRequestSecrets(format, payload, table)
		if failure != "" {
			return refuse(failure)
		}
		if masked != nil {
			result.Payload = masked
			if len(original) > 0 {
				result.OriginalRequest = masked
			}
		}
	} else {
		maskedOriginal, _, failure := maskRequestSecrets(format, original, table)
		if failure != "" {
			return refuse(failure)
		}
		maskedPayload, _, failure := maskRequestSecrets(format, payload, table)
		if failure != "" {
			return refuse(failure)
		}
		if maskedOriginal != nil {
			result.OriginalRequest = maskedOriginal
		}
		if maskedPayload != nil {
			result.Payload = maskedPayload
		}
	}
	if len(table.values) == 0 {
		return result
	}
	setSecretMaskingSnapshot(ctx, table.snapshot())
	// The reply is put back the way the request was changed, in the opposite
	// order: the secrets first, so the word guard sees the reply as the client
	// will.
	result.Reply = chainReplies(newSecretReply(table, format), result.Reply)
	return result
}

// warnSecretMasking logs a problem with a tenant's masking, at most once in a
// while per tenant. It never names a secret.
func (m *RuntimeModerator) warnSecretMasking(tenantID, message string) {
	ok, dropped := m.logs.allow("secret_masking\x00"+tenantID, wordHitLogInterval, time.Now())
	if !ok {
		return
	}
	log.WithFields(log.Fields{"tenant_id": tenantID, "suppressed": dropped}).Warn(message)
}

// SecretMaskingSnapshot is what secret masking did to one request, for its
// request log. It counts secrets; it never holds one, or a placeholder.
type SecretMaskingSnapshot struct {
	// Masked is how many different secrets were replaced.
	Masked int `json:"masked,omitempty"`
	// Kinds counts them by kind (API_KEY, JWT, TOKEN, PASSWORD, SECRET).
	Kinds map[string]int `json:"kinds,omitempty"`
	// Refused says why the request was not sent, when a secret could not be
	// replaced.
	Refused string `json:"refused,omitempty"`
}

// snapshot counts what the table replaced.
func (t *secretTable) snapshot() *SecretMaskingSnapshot {
	s := &SecretMaskingSnapshot{Masked: len(t.values), Kinds: make(map[string]int)}
	for p := range t.values {
		// {{KIND_tag}}: the tag holds no underscore.
		kind := strings.TrimPrefix(p, "{{")
		if cut := strings.LastIndexByte(kind, '_'); cut > 0 {
			kind = kind[:cut]
		}
		s.Kinds[kind]++
	}
	return s
}

const ginSecretMaskingSnapshotKey = "content_moderation_secret_masking_snapshot"

func setSecretMaskingSnapshot(ctx context.Context, snapshot *SecretMaskingSnapshot) {
	if ctx == nil || snapshot == nil {
		return
	}
	if ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context); ginCtx != nil {
		ginCtx.Set(ginSecretMaskingSnapshotKey, snapshot)
	}
}

// SecretMaskingSnapshotFromGin returns what secret masking did to the request,
// if it did anything.
func SecretMaskingSnapshotFromGin(ginCtx *gin.Context) (*SecretMaskingSnapshot, bool) {
	if ginCtx == nil {
		return nil, false
	}
	value, exists := ginCtx.Get(ginSecretMaskingSnapshotKey)
	if !exists {
		return nil, false
	}
	snapshot, ok := value.(*SecretMaskingSnapshot)
	return snapshot, ok && snapshot != nil
}
