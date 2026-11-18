package authfiles

import (
	"fmt"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// XAIEndpointPayload returns the management-facing Grok endpoint mode for
// xAI OAuth auth files (using_api: false=Build/CLI, true=official API).
func XAIEndpointPayload(auth *coreauth.Auth) map[string]any {
	if !isXAIEndpointEditableAuth(auth) {
		return nil
	}
	return map[string]any{
		"using_api": xaiUsingAPIFromAuth(auth),
	}
}

func ensureXAIEndpointEditable(auth *coreauth.Auth) error {
	if !isXAIEndpointEditableAuth(auth) {
		return fmt.Errorf("using_api is only supported for xAI OAuth auth files")
	}
	return nil
}

func isXAIEndpointEditableAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "xai" && provider != "grok" && provider != "x-ai" {
		return false
	}
	if accountType, _ := auth.AccountInfo(); strings.EqualFold(accountType, "oauth") {
		return true
	}
	if auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "oauth") {
		return true
	}
	return strings.EqualFold(MetadataString(auth.Metadata, "auth_kind", "authKind"), "oauth")
}

// xaiUsingAPIFromAuth mirrors runtime routing defaults: attributes first,
// then metadata; OAuth without a value defaults to Build (false).
func xaiUsingAPIFromAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["using_api"]); raw != "" {
			if usingAPI, err := strconv.ParseBool(raw); err == nil {
				return usingAPI
			}
		}
	}
	if value, ok := MetadataBoolPresence(auth.Metadata, "using_api", "using-api", "usingApi"); ok {
		return value
	}
	return false
}

func ensureAttributes(auth *coreauth.Auth) map[string]string {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	return auth.Attributes
}
