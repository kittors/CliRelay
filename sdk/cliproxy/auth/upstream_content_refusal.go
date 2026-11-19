package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Upstream content refusals and provider access freezes.
//
// Some upstreams run a safety classifier over each request and refuse it for
// what it asks, not for how it is formed. Codex answers a request its cyber
// classifier flags with a stream that opens normally (response.created) and
// then fails with error.code "cyber_policy". Every account behind that upstream
// runs the same classifier, so trying another one only hands the same content
// to more of the pool, and the account did nothing wrong, so it must not cool
// down. The host hears of each refusal (UpstreamRefusalObserver) and may freeze
// the requester's access to that provider; selection then leaves the provider's
// credentials out of the requester's requests (FrozenProvidersMetadataKey).

// UpstreamContentRefusal is an upstream refusing a request for its content.
type UpstreamContentRefusal struct {
	// Code is the upstream's error code, e.g. "cyber_policy".
	Code string
	// Message is the upstream's message, verbatim.
	Message string
}

// upstreamContentRefusalCodes are the error codes that mean an upstream safety
// refusal. Only codes seen on live traffic belong here: the host freezes the
// requester's access on one, so a code that may also mean a malformed request
// must not be added on a guess.
//
//   - cyber_policy (Codex): "This content was flagged for possible
//     cybersecurity risk. ...", type invalid_request, streamed after
//     response.created. Seen on hk-155 on 2026-11-13 for gpt-6.1-sol and
//     gpt-6-astra, the only safety refusal among 2793 failed requests of the
//     preceding four days.
var upstreamContentRefusalCodes = map[string]struct{}{
	"cyber_policy": {},
}

// UpstreamContentRefusalOf reports whether err is an upstream refusing the
// request for its content, and which refusal it is.
func UpstreamContentRefusalOf(err error) (UpstreamContentRefusal, bool) {
	if err == nil {
		return UpstreamContentRefusal{}, false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if code, ok := upstreamContentRefusalCode(authErr.Code); ok {
			return UpstreamContentRefusal{Code: code, Message: strings.TrimSpace(authErr.Message)}, true
		}
		if refusal, ok := upstreamContentRefusalFromBody(authErr.Message); ok {
			return refusal, true
		}
	}
	return upstreamContentRefusalFromBody(err.Error())
}

func isUpstreamContentRefusal(err error) bool {
	_, ok := UpstreamContentRefusalOf(err)
	return ok
}

func upstreamContentRefusalCode(raw string) (string, bool) {
	code := strings.ToLower(strings.TrimSpace(raw))
	_, ok := upstreamContentRefusalCodes[code]
	return code, ok
}

// upstreamContentRefusalFromBody reads the error body an executor kept: the
// OpenAI shape {"error":{"code":...}}, a response.failed event
// {"response":{"error":{...}}}, or a bare {"code":...}.
func upstreamContentRefusalFromBody(body string) (UpstreamContentRefusal, bool) {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") || !gjson.Valid(body) {
		return UpstreamContentRefusal{}, false
	}
	root := gjson.Parse(body)
	for _, node := range []gjson.Result{root.Get("error"), root.Get("response.error"), root} {
		if !node.IsObject() {
			continue
		}
		if code, ok := upstreamContentRefusalCode(node.Get("code").String()); ok {
			return UpstreamContentRefusal{Code: code, Message: strings.TrimSpace(node.Get("message").String())}, true
		}
	}
	return UpstreamContentRefusal{}, false
}

// UpstreamRefusalEvent is one attempt an upstream refused for its content.
type UpstreamRefusalEvent struct {
	AuthID    string
	AuthLabel string
	Provider  string
	Model     string
	Refusal   UpstreamContentRefusal
}

// UpstreamRefusalObserver hears of every attempt an upstream refused for its
// content. It is called on the request's goroutine once the attempt's result is
// recorded, with the request's context (which carries what the access provider
// learned about the API key): an implementation must return at once and do any
// storage work elsewhere.
type UpstreamRefusalObserver interface {
	OnUpstreamContentRefusal(ctx context.Context, event UpstreamRefusalEvent)
}

// SetUpstreamRefusalObserver installs the observer of upstream content
// refusals; nil removes it.
func (m *Manager) SetUpstreamRefusalObserver(observer UpstreamRefusalObserver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.upstreamRefusalObserver = observer
	m.mu.Unlock()
}

func (m *Manager) observeUpstreamRefusal(ctx context.Context, result Result) {
	if m == nil || result.Success || result.Error == nil {
		return
	}
	refusal, ok := UpstreamContentRefusalOf(result.Error)
	if !ok {
		return
	}
	m.mu.RLock()
	observer := m.upstreamRefusalObserver
	label := ""
	if auth := m.auths[result.AuthID]; auth != nil {
		label = auth.Label
	}
	m.mu.RUnlock()
	if observer == nil {
		return
	}
	observer.OnUpstreamContentRefusal(ctx, UpstreamRefusalEvent{
		AuthID:    result.AuthID,
		AuthLabel: label,
		Provider:  result.Provider,
		Model:     result.Model,
		Refusal:   refusal,
	})
}

// ProviderAccessFrozenCode is the error of a request refused because the
// requester's access is frozen for every provider that could serve it.
const ProviderAccessFrozenCode = "provider_access_frozen"

// frozenProvidersFromMetadata reads FrozenProvidersMetadataKey: the providers
// the requester may not use, each with the notice the requester is shown.
func frozenProvidersFromMetadata(meta map[string]any) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	var parsed map[string]string
	switch typed := meta[cliproxyexecutor.FrozenProvidersMetadataKey].(type) {
	case map[string]string:
		parsed = typed
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		// The host writes this value with encoding/json; anything else is a bug
		// upstream of selection, not a freeze to guess at.
		if err := json.Unmarshal([]byte(typed), &parsed); err != nil {
			return nil
		}
	default:
		return nil
	}
	out := make(map[string]string, len(parsed))
	for provider, notice := range parsed {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(notice)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// frozenNotice returns the notice of candidate's provider when the requester's
// access to it is frozen.
func (s selectionScope) frozenNotice(candidate *Auth) (string, bool) {
	if len(s.frozenProviders) == 0 || candidate == nil {
		return "", false
	}
	notice, frozen := s.frozenProviders[strings.ToLower(strings.TrimSpace(candidate.Provider))]
	return notice, frozen
}

// providerAccessFrozenError answers a request no credential can take because
// the requester's access to the providers that serve it is frozen. The notice
// is the host's: it says what was frozen, why, and who can lift it.
func providerAccessFrozenError(notices map[string]string) *Error {
	providers := make([]string, 0, len(notices))
	for provider := range notices {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	seen := make(map[string]struct{}, len(providers))
	parts := make([]string, 0, len(providers))
	for _, provider := range providers {
		notice := notices[provider]
		if notice == "" {
			notice = "Access to " + provider + " is frozen for this account. Contact your administrator to restore it."
		}
		if _, dup := seen[notice]; dup {
			continue
		}
		seen[notice] = struct{}{}
		parts = append(parts, notice)
	}
	return &Error{Code: ProviderAccessFrozenCode, Message: strings.Join(parts, " "), HTTPStatus: http.StatusForbidden}
}

func isProviderAccessFrozen(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil && authErr.Code == ProviderAccessFrozenCode
}
