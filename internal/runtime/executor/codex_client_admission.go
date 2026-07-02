package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func enforceCodexClientAdmission(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) error {
	return enforceCodexClientAdmissionWithHeaders(ctx, cfg, auth, codexAdmissionHeadersFromContext(ctx))
}

func enforceCodexClientAdmissionForRequest(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request) error {
	headers := codexAdmissionHeadersFromContext(ctx)
	if headers == nil && req != nil {
		headers = req.Header
	}
	return enforceCodexClientAdmissionWithHeaders(ctx, cfg, auth, headers)
}

func enforceCodexClientAdmissionWithHeaders(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, headers http.Header) error {
	admissionCfg := codexClientAdmissionConfig(cfg, auth)
	result := codexadmission.EvaluateHeaders(headers, admissionCfg)
	if !result.Enabled || result.Matched {
		return nil
	}
	logCodexClientAdmissionRejected(auth, admissionCfg, result, headers)
	return statusErr{code: http.StatusForbidden, msg: "codex_cli_only restriction: only Codex official clients are allowed"}
}

func codexClientAdmissionConfig(cfg *config.Config, auth *cliproxyauth.Auth) codexadmission.Config {
	if !isCodexOAuthAdmissionAuth(auth) || auth.Metadata == nil {
		return codexadmission.Config{}
	}
	enabled, ok := auth.Metadata["codex_cli_only"].(bool)
	if !ok || !enabled {
		return codexadmission.Config{}
	}
	accountPresets := codexadmission.FilterKnownAllowedClientPresets(codexAllowedClientPresetMetadata(auth.Metadata["codex_cli_only_allowed_clients"]))
	var globalPresets []string
	if cfg != nil {
		globalPresets = config.CleanCodexOAuthAdmission(cfg.CodexOAuthAdmission).AllowedClientPresets
	}
	return codexadmission.Config{
		Enabled:                    true,
		AllowedClientPresets:       accountPresets,
		GlobalAllowedClientPresets: globalPresets,
	}
}

func isCodexOAuthAdmissionAuth(auth *cliproxyauth.Auth) bool {
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

func codexAdmissionHeadersFromContext(ctx context.Context) http.Header {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil || ginCtx.Request == nil {
		return nil
	}
	return ginCtx.Request.Header
}

func logCodexClientAdmissionRejected(auth *cliproxyauth.Auth, cfg codexadmission.Config, result codexadmission.Result, headers http.Header) {
	fields := log.Fields{
		"component":               "executor.codex_client_admission",
		"reason":                  result.Reason,
		"matched_preset":          result.MatchedPreset,
		"account_allowed_presets": cfg.AllowedClientPresets,
		"global_allowed_presets":  cfg.GlobalAllowedClientPresets,
	}
	if auth != nil {
		fields["auth_id"] = auth.ID
		fields["auth_index"] = auth.EnsureIndex()
		if auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				fields["account_id"] = strings.TrimSpace(accountID)
			}
			if email, ok := auth.Metadata["email"].(string); ok {
				fields["email"] = strings.TrimSpace(email)
			}
		}
	}
	if headers != nil {
		fields["request_user_agent"] = strings.TrimSpace(headers.Get("User-Agent"))
		fields["request_originator"] = strings.TrimSpace(headers.Get("Originator"))
	}
	log.WithFields(fields).Warn("codex oauth client admission rejected request")
}
