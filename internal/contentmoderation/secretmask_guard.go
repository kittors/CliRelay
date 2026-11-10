package contentmoderation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// GuardRequest implements coreauth.RequestGuard: the word guard (guardWords)
// and then, for a request it lets through, the tenant's secret masking
// (maskSecrets), on the request as the word guard left it.
//
// The two fail differently. The word guard fails open: a panic in it leaves
// the request as it came, as any other failure of its dependencies does, and
// masking still runs. Masking fails closed: a tenant that switched it on asked
// for its secrets to stay here, so a request masking could not finish is not
// sent (maskSecretsSafely). Each recovers its own panic; the SDK's recovery
// around the whole guard would send the request as it came.
func (m *RuntimeModerator) GuardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) coreauth.RequestGuardResult {
	result := m.guardWordsSafely(ctx, req, opts)
	if result.Blocked {
		return result
	}
	return m.maskSecretsSafely(ctx, req, opts, result)
}

func (m *RuntimeModerator) guardWordsSafely(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result coreauth.RequestGuardResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logGuardPanic("word guard", recovered)
			result = coreauth.RequestGuardResult{}
		}
	}()
	return m.guardWords(ctx, req, opts)
}

func (m *RuntimeModerator) maskSecretsSafely(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, words coreauth.RequestGuardResult) (result coreauth.RequestGuardResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logGuardPanic("secret masking", recovered)
			setSecretMaskingSnapshot(ctx, &SecretMaskingSnapshot{Refused: "masking_failed"})
			result = coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusServiceUnavailable,
				Message: "Secret masking failed on this request, so it was not sent: nothing reached the upstream. Try again; if it keeps failing, ask an administrator."}
		}
	}()
	return m.maskSecrets(ctx, req, opts, words)
}

// guardPanicLogs spaces the log lines of a guard that panics on every request.
var guardPanicLogs logLimiter

func (m *RuntimeModerator) logGuardPanic(part string, recovered any) {
	ok, dropped := guardPanicLogs.allow("guard_panic\x00"+part, wordHitLogInterval, time.Now())
	if !ok {
		return
	}
	log.WithFields(log.Fields{"suppressed": dropped, "panic": fmt.Sprint(recovered)}).
		Errorf("content moderation: the %s panicked\n%s", part, debug.Stack())
}

func (m *RuntimeModerator) secretMaskingRegistry() *SecretMaskingRegistry {
	if m != nil && m.secretMasking != nil {
		return m.secretMasking
	}
	return SecretMasking()
}

// secretMaskingTestHook, when set, runs as masking starts: a test makes it
// panic to see that a request masking could not finish is not sent.
var secretMaskingTestHook func()

// privacyMaskOffEnv switches privacy masking off for every tenant when set to
// "off" at start: the operator's way back if masking itself goes wrong, without
// touching any tenant's settings. Requests are then sent as they came, so it
// is logged loudly, and the settings page is told.
const privacyMaskOffEnv = "CLIRELAY_PRIVACY_MASK"

var privacyMaskOff = sync.OnceValue(func() bool {
	off := strings.EqualFold(strings.TrimSpace(os.Getenv(privacyMaskOffEnv)), "off")
	if off {
		log.Warnf("privacy masking is switched off for every tenant (%s=off): requests are sent without masking, whatever the tenants set", privacyMaskOffEnv)
	}
	return off
})

// PrivacyMaskingDisabledByServer says the operator switched masking off for
// every tenant.
func PrivacyMaskingDisabledByServer() bool { return privacyMaskOff() }

// maskScopes are the senders whose requests masking remembers (see
// privacymask.Scope): one per portal user, or per API key without one, of a
// tenant. In memory only.
var maskScopes = privacymask.NewScopes()

// maskScopeID names the scope of a request: its tenant, then its portal user,
// else its API key, else the tenant as a whole.
func maskScopeID(ctx context.Context, tenantID string) string {
	switch endUser, key := wordGuardEndUserID(ctx), wordGuardKeyID(ctx); {
	case endUser != "":
		return tenantID + "\x00u:" + endUser
	case key != "":
		return tenantID + "\x00k:" + key
	}
	return tenantID + "\x00t"
}

// maskSecrets replaces what privacy masking finds in a request with
// placeholders, under its tenant's policy, and adds the reply filter that puts
// the values back.
// words is what the word guard made of the request; the bodies it rewrote are
// the ones masked.
//
// A request whose values cannot all be replaced is not sent: the tenant asked
// for them to stay here.
func (m *RuntimeModerator) maskSecrets(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, result coreauth.RequestGuardResult) coreauth.RequestGuardResult {
	if secretMaskingTestHook != nil {
		secretMaskingTestHook()
	}
	if PrivacyMaskingDisabledByServer() {
		return result
	}
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	settings, known := m.secretMaskingRegistry().Lookup(tenantID)
	if !known {
		// Never read, and the store did not answer now: masking may be on.
		m.warnSecretMasking(tenantID, "secret masking settings could not be read; requests are refused until they can be")
		setSecretMaskingSnapshot(ctx, &SecretMaskingSnapshot{Refused: "settings_unreadable"})
		return coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusServiceUnavailable,
			Message: "This tenant's secret masking settings could not be read just now, so the request was not sent. Try again in a moment."}
	}
	endUserID, keyID := wordGuardEndUserID(ctx), wordGuardKeyID(ctx)
	options := settings.policy().options()
	if !options.Any() {
		return result
	}
	key, ok := settings.placeholderKey(tenantID)
	if !ok {
		// Never saved so: setting masking up makes the key. A row edited by hand,
		// or one sealed under a data key this server does not have, does not get
		// the request sent unmasked.
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
	masker := privacymask.NewMasker(key, options, maskScopes.Get(maskScopeID(ctx, tenantID)), req.Model)
	notes := newMaskNotes()
	snapshotNow := func() *SecretMaskingSnapshot {
		snapshot := newSecretMaskingSnapshot(masker, notes)
		snapshot.tenantID, snapshot.endUserID, snapshot.apiKeyID, snapshot.model = tenantID, endUserID, keyID, req.Model
		return snapshot
	}
	format := opts.SourceFormat
	refuse := func(failure string) coreauth.RequestGuardResult {
		snapshot := snapshotNow()
		snapshot.Refused = failure
		setSecretMaskingSnapshot(ctx, snapshot)
		m.warnSecretMasking(tenantID, "secret masking refused a request: "+failure)
		message := "The request holds a secret that secret masking cannot replace (" + failure + "), so it was not sent. Remove the secret, or ask an administrator to switch secret masking off."
		if failure == maskFailureUnreadable {
			message = UnreadableBodyMessage
		}
		return coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusBadRequest, Message: message}
	}
	if len(original) == 0 || sameStorage(original, payload) {
		// One body: both names keep pointing at one slice, which is how the
		// executors know not to translate it twice.
		masked, failure := maskRequest(format, payload, masker, false, notes)
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
		maskedOriginal, failure := maskRequest(format, original, masker, false, notes)
		if failure != "" {
			return refuse(failure)
		}
		maskedPayload, failure := maskRequest(format, payload, masker, true, nil)
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
	snapshot := snapshotNow()
	if blocked := masker.Blocked(); len(blocked) > 0 {
		snapshot.Refused = "blocked"
		setSecretMaskingSnapshot(ctx, snapshot)
		return coreauth.RequestGuardResult{Blocked: true, HTTPStatus: http.StatusBadRequest, Message: blockedMessage(blocked)}
	}
	restorer := masker.Restorer()
	if len(masker.Values()) == 0 && restorer == nil {
		return result
	}
	masker.Commit()
	snapshot.restorer = restorer
	setSecretMaskingSnapshot(ctx, snapshot)
	if restorer == nil {
		return result // only values sent as written: nothing to put back
	}
	// The reply is put back the way the request was changed, in the opposite
	// order: the secrets first, so the word guard sees the reply as the client
	// will.
	result.Reply = chainReplies(newSecretReply(restorer, format), result.Reply)
	return result
}

// blockedMessage tells the client why its request was held back: the kinds
// and what may be said of each value (its first characters and length), never
// the value.
func blockedMessage(blocked []privacymask.Value) string {
	var parts []string
	for i, v := range blocked {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(blocked)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%s…, %d characters)", v.Kind, v.Head, v.Len))
	}
	return "The request was not sent: it holds " + strings.Join(parts, ", ") +
		", which secret masking is set to hold back. Nothing reached the upstream. Remove it, or ask an administrator to mask it instead."
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
// request log and its record. It describes the values; it never holds one. The
// placeholders and value ids are kept for the record only: the request log
// leaves them out.
type SecretMaskingSnapshot struct {
	// Key is the request's record (MaskRecord.Key), "" for a request without
	// one.
	Key string `json:"key,omitempty"`
	// Masked is how many different values were replaced, and Places in how
	// many places.
	Masked int `json:"masked,omitempty"`
	Places int `json:"places,omitempty"`
	// Kinds counts the different values replaced by kind (API_KEY, TOKEN,
	// PASSWORD, SECRET, PRIVATE_KEY, …).
	Kinds map[string]int `json:"kinds,omitempty"`
	// Warned is how many different values went as written because their kind
	// is set to warn.
	Warned int `json:"warned,omitempty"`
	// Values describes each value found: its kind, what found it, what was done
	// with it, its first characters, its length and its places.
	Values []privacymask.Value `json:"values,omitempty"`
	// TookMicros is how long finding them took.
	TookMicros int64 `json:"took_us,omitempty"`
	// Refused says why the request was not sent: a value that could not be
	// replaced, one of a kind set to block ("blocked"), unreadable settings or
	// a failure of masking itself.
	Refused string `json:"refused,omitempty"`

	// restorer counts the places the reply had a value put back; the reply may
	// still be streaming when the request log is written, and counts as it goes.
	restorer *privacymask.Restorer
	// for the record: who sent it, what for, and what masking noted
	tenantID, endUserID, apiKeyID, model string
	notes                                *maskNotes
	at                                   time.Time
	// where it was sent, noted by the executor (NoteSecretMaskingRoute)
	route struct {
		sync.Mutex
		channel, provider string
	}
}

func newSecretMaskingSnapshot(masker *privacymask.Masker, notes *maskNotes) *SecretMaskingSnapshot {
	s := &SecretMaskingSnapshot{Places: masker.Places(), Values: masker.Values(), TookMicros: masker.Took().Microseconds(), notes: notes, at: time.Now()}
	if len(s.Values) > 0 {
		s.Key = newMaskKey(s.at)
	}
	for _, v := range s.Values {
		if v.Action == privacymask.Warn {
			s.Warned++
			continue
		}
		if s.Kinds == nil {
			s.Kinds = make(map[string]int)
		}
		s.Masked++
		s.Kinds[v.Kind]++
	}
	return s
}

// NoteSecretMaskingRoute notes where a masked request was sent: the channel of
// the account chosen and its provider. The last one noted is the one that
// answered.
func NoteSecretMaskingRoute(ctx context.Context, channel, provider string) {
	if ctx == nil {
		return
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	snapshot, ok := SecretMaskingSnapshotFromGin(ginCtx)
	if !ok || snapshot.Key == "" {
		return
	}
	snapshot.route.Lock()
	snapshot.route.channel, snapshot.route.provider = strings.TrimSpace(channel), strings.TrimSpace(provider)
	snapshot.route.Unlock()
}

// Record is the request's record once it is over, nil when masking found
// nothing in it. client is what sent it (MaskClient).
func (s *SecretMaskingSnapshot) Record(client string) *MaskRecord {
	if s == nil || s.Key == "" || len(s.Values) == 0 {
		return nil
	}
	r := &MaskRecord{
		Key: s.Key, At: s.at, TenantID: s.tenantID, EndUserID: s.endUserID, APIKeyID: s.apiKeyID, Model: s.model,
		Client: client, Places: s.Places, Distinct: len(s.Values), Warned: s.Warned, TookMicros: s.TookMicros,
		Blocked: s.Refused != "",
	}
	s.route.Lock()
	if !r.Blocked {
		r.Channel, r.Provider = s.route.channel, s.route.provider
	}
	s.route.Unlock()
	r.Values = make([]MaskRecordValue, 0, len(s.Values))
	for _, v := range s.Values {
		value := MaskRecordValue{ID: v.ID, Kind: v.Kind, By: v.By, Action: v.Action, Head: v.Head, Len: v.Len, N: v.N, Placeholder: v.Placeholder}
		if v.Placeholder != "" {
			value.Restored = s.restorer.RestoredOf(v.Placeholder)
			r.Restored += value.Restored
		}
		if s.notes != nil {
			value.Field = s.notes.fields[v.ID]
		}
		r.Values = append(r.Values, value)
	}
	if s.notes != nil {
		r.Snippets = s.notes.snippets
	}
	return r
}

// MarshalJSON adds how many places the reply had a value put back so far.
func (s *SecretMaskingSnapshot) MarshalJSON() ([]byte, error) {
	type plain SecretMaskingSnapshot
	return json.Marshal(struct {
		*plain
		Restored int `json:"restored,omitempty"`
	}{(*plain)(s), s.restorer.Restored()})
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
