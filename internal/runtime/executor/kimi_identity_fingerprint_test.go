package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func kimiFingerprintRequest(t *testing.T, inbound map[string]string) *http.Request {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for key, value := range inbound {
		ginCtx.Request.Header.Set(key, value)
	}
	req := httptest.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	return req.WithContext(context.WithValue(req.Context(), util.ContextKeyGin, ginCtx))
}

func kimiFingerprintConfig() *config.Config {
	return &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Kimi: config.NormalizeKimiIdentityFingerprint(config.KimiIdentityFingerprintConfig{}),
		},
	}
}

func TestApplyKimiHeadersAdoptsLearnedClientIdentity(t *testing.T) {
	req := kimiFingerprintRequest(t, map[string]string{
		"User-Agent":         "KimiCLI/1.12.0",
		"X-Msh-Platform":     "kimi_cli",
		"X-Msh-Version":      "1.12.0",
		"X-Msh-Device-Name":  "starship",
		"X-Msh-Device-Model": "darwin arm64",
		"X-Msh-Device-Id":    "device-from-client",
	})
	auth := &cliproxyauth.Auth{ID: "kimi-learned-account", Provider: "kimi"}

	applyKimiHeadersWithAuth(req, "token", false, auth, kimiFingerprintConfig())

	for header, want := range map[string]string{
		"User-Agent":         "KimiCLI/1.12.0",
		"X-Msh-Platform":     "kimi_cli",
		"X-Msh-Version":      "1.12.0",
		"X-Msh-Device-Name":  "starship",
		"X-Msh-Device-Model": "darwin arm64",
		// No credential-scoped id exists, so the client's own device is used rather
		// than the host fallback every such credential would otherwise share.
		"X-Msh-Device-Id": "device-from-client",
	} {
		if got := req.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestApplyKimiHeadersKeepsCredentialDeviceIDOverLearnedOne(t *testing.T) {
	req := kimiFingerprintRequest(t, map[string]string{
		"User-Agent":      "KimiCLI/1.12.0",
		"X-Msh-Platform":  "kimi_cli",
		"X-Msh-Device-Id": "device-from-client",
	})
	auth := &cliproxyauth.Auth{
		ID:       "kimi-bound-device-account",
		Provider: "kimi",
		Metadata: map[string]any{"device_id": "device-from-login"},
	}

	applyKimiHeadersWithAuth(req, "token", false, auth, kimiFingerprintConfig())

	// The login-time device is the one Moonshot associated with the account.
	if got := req.Header.Get("X-Msh-Device-Id"); got != "device-from-login" {
		t.Fatalf("X-Msh-Device-Id = %q, want the credential's own device id", got)
	}
	if got := req.Header.Get("User-Agent"); got != "KimiCLI/1.12.0" {
		t.Fatalf("User-Agent = %q, want the learned client version", got)
	}
}

func TestApplyKimiHeadersIgnoresForeignClientIdentity(t *testing.T) {
	req := kimiFingerprintRequest(t, map[string]string{
		"User-Agent":      "curl/8.7.1",
		"X-Msh-Device-Id": "not-a-kimi-device",
	})
	auth := &cliproxyauth.Auth{ID: "kimi-foreign-client-account", Provider: "kimi"}

	applyKimiHeadersWithAuth(req, "token", false, auth, kimiFingerprintConfig())

	if got := req.Header.Get("User-Agent"); got != config.DefaultKimiFingerprintUserAgent {
		t.Fatalf("User-Agent = %q, want the builtin kimi-cli identity", got)
	}
	if got := req.Header.Get("X-Msh-Device-Id"); got == "not-a-kimi-device" {
		t.Fatal("a non-kimi caller must not set the account's outbound device id")
	}
	// The host fallback still applies, matching the pre-fingerprint behaviour.
	if got := req.Header.Get("X-Msh-Device-Name"); got != getKimiHostname() {
		t.Fatalf("X-Msh-Device-Name = %q, want the host fallback", got)
	}
}

func TestApplyKimiHeadersWithFingerprintDisabledKeepsLegacyDefaults(t *testing.T) {
	req := kimiFingerprintRequest(t, map[string]string{
		"User-Agent":     "KimiCLI/1.12.0",
		"X-Msh-Platform": "kimi_cli",
	})
	cfg := &config.Config{
		KimiHeaderDefaults: config.KimiHeaderDefaults{UserAgent: "KimiCLI/1.8.0", Version: "1.8.0"},
	}
	auth := &cliproxyauth.Auth{ID: "kimi-fingerprint-off-account", Provider: "kimi"}

	applyKimiHeadersWithAuth(req, "token", false, auth, cfg)

	if got := req.Header.Get("User-Agent"); got != "KimiCLI/1.8.0" {
		t.Fatalf("User-Agent = %q, want the configured header defaults when the fingerprint is off", got)
	}
	if got := req.Header.Get("X-Msh-Version"); got != "1.8.0" {
		t.Fatalf("X-Msh-Version = %q, want the configured header defaults", got)
	}
}

func TestApplyKimiHeadersFingerprintKeepsLegacyHeaderDefaultsAsPreset(t *testing.T) {
	req := kimiFingerprintRequest(t, nil)
	cfg := kimiFingerprintConfig()
	cfg.KimiHeaderDefaults = config.KimiHeaderDefaults{UserAgent: "KimiCLI/1.8.0", Version: "1.8.0"}
	auth := &cliproxyauth.Auth{ID: "kimi-legacy-preset-account", Provider: "kimi"}

	applyKimiHeadersWithAuth(req, "token", false, auth, cfg)

	// Enabling the fingerprint must not silently replace an operator's existing
	// kimi-header-defaults with the builtin template.
	if got := req.Header.Get("User-Agent"); got != "KimiCLI/1.8.0" {
		t.Fatalf("User-Agent = %q, want the legacy header defaults to survive", got)
	}
	if got := req.Header.Get("X-Msh-Platform"); got != config.DefaultKimiFingerprintPlatform {
		t.Fatalf("X-Msh-Platform = %q, want the builtin value for the field legacy config left unset", got)
	}
}

func TestKimiFingerprintCustomHeadersCannotOverrideManagedHeaders(t *testing.T) {
	headers := http.Header{}
	applyKimiIdentityFingerprintHeaders(headers, config.KimiIdentityFingerprintConfig{
		Enabled:   true,
		UserAgent: "KimiCLI/1.12.0",
		CustomHeaders: map[string]string{
			"X-Msh-Device-Id": "spoofed",
			"X-Msh-Trace":     "kept",
		},
	})

	if got := headers.Get("X-Msh-Device-Id"); got != "" {
		t.Fatalf("X-Msh-Device-Id = %q, want the managed header left untouched", got)
	}
	if got := headers.Get("X-Msh-Trace"); got != "kept" {
		t.Fatalf("X-Msh-Trace = %q, want unmanaged custom headers applied", got)
	}
}
