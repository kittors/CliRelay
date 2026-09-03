package authfiles

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	metadataKeyCodexConvergenceMode = "codex_convergence_mode"
)

// CodexConvergenceModePayload returns the configured per-account convergence mode if present and valid.
func CodexConvergenceModePayload(auth *coreauth.Auth) string {
	if !isCodexOAuthAdmissionAuth(auth) || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{metadataKeyCodexConvergenceMode, "convergence_mode", "codex-convergence-mode"} {
		if raw, ok := auth.Metadata[key]; ok {
			if s, isStr := raw.(string); isStr {
				trimmed := strings.TrimSpace(strings.ToLower(s))
				if config.IsValidCodexFingerprintConvergenceMode(trimmed) {
					return trimmed
				}
			}
		}
	}
	return ""
}

func ensureCodexConvergenceModeEditable(auth *coreauth.Auth, mode string) error {
	if !isCodexOAuthAdmissionAuth(auth) {
		return fmt.Errorf("codex convergence mode is only supported for Codex OAuth auth files")
	}
	trimmed := strings.TrimSpace(strings.ToLower(mode))
	if trimmed != "" && !config.IsValidCodexFingerprintConvergenceMode(trimmed) {
		return fmt.Errorf("invalid codex convergence mode %q: must be empty, off, device, session, or full", mode)
	}
	return nil
}
