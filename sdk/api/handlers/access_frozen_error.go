package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// providerAccessFrozenError is the client-facing form of a request refused
// because the requester's access to every provider that could serve it is
// frozen (sdk/cliproxy/auth upstream_content_refusal.go). Its text is the
// OpenAI-shaped body; the other protocols rewrite it as any other error.
type providerAccessFrozenError struct {
	body string
}

func (e *providerAccessFrozenError) Error() string   { return e.body }
func (e *providerAccessFrozenError) StatusCode() int { return http.StatusForbidden }

// providerAccessFrozenMessage turns a freeze refusal into the error a client
// is answered with, or returns nil when err is not one.
//
// Rendered from its status alone, a 403 goes out with the code
// insufficient_quota, which clients such as Codex turn into a quota message of
// their own and never show the notice. The body carries the code
// provider_access_frozen and the notice, which says what is frozen, why, and
// who can lift it.
func providerAccessFrozenMessage(err error) *StreamErrorMessage {
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil || authErr.Code != coreauth.ProviderAccessFrozenCode {
		return nil
	}
	body, errMarshal := json.Marshal(map[string]any{"error": map[string]any{
		"message": authErr.Message,
		"type":    "permission_error",
		"code":    coreauth.ProviderAccessFrozenCode,
	}})
	if errMarshal != nil {
		body = []byte(`{"error":{"message":"Access to this upstream is frozen for this account. Contact your administrator to restore it.","type":"permission_error","code":"` + coreauth.ProviderAccessFrozenCode + `"}}`)
	}
	return &StreamErrorMessage{
		StatusCode: http.StatusForbidden,
		Error:      &providerAccessFrozenError{body: string(body)},
	}
}
