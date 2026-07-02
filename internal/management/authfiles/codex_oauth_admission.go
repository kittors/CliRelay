package authfiles

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func CodexOAuthAdmissionPayload(auth *coreauth.Auth) map[string]any {
	if !isCodexOAuthAdmissionAuth(auth) {
		return nil
	}
	enabled, _ := auth.Metadata["codex_cli_only"].(bool)
	allowedClients := codexadmission.FilterKnownAllowedClientPresets(codexAllowedClientPresetMetadata(auth.Metadata["codex_cli_only_allowed_clients"]))
	return map[string]any{
		"enabled":                   enabled,
		"allowed_clients":           allowedClients,
		"available_allowed_clients": codexadmission.AvailableAllowedClientPresets(),
	}
}

func ensureCodexOAuthAdmissionEditable(auth *coreauth.Auth) error {
	if !isCodexOAuthAdmissionAuth(auth) {
		return fmt.Errorf("codex oauth admission is only supported for Codex OAuth auth files")
	}
	return nil
}

func isCodexOAuthAdmissionAuth(auth *coreauth.Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	return true
}

func codexAllowedClientPresetMetadata(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
