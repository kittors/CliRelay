package config

import (
	"fmt"
	"strings"
)

const (
	ClaudeFingerprintProfileDefault       = ""
	ClaudeFingerprintProfileClaudeCodeCLI = "claude-code-cli"
	claudeFingerprintProfileOAuthCLIAlias = "oauth-cli"
)

func NormalizeClaudeFingerprintProfile(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ClaudeFingerprintProfileClaudeCodeCLI, claudeFingerprintProfileOAuthCLIAlias:
		return ClaudeFingerprintProfileClaudeCodeCLI, true
	case ClaudeFingerprintProfileDefault:
		return ClaudeFingerprintProfileDefault, true
	default:
		return ClaudeFingerprintProfileDefault, false
	}
}

func ValidateClaudeFingerprintProfile(raw string) error {
	if _, ok := NormalizeClaudeFingerprintProfile(raw); !ok {
		return fmt.Errorf("unsupported fingerprint-profile %q (supported: %q or empty)", strings.TrimSpace(raw), ClaudeFingerprintProfileClaudeCodeCLI)
	}
	return nil
}
