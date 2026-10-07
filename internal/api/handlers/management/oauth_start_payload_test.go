package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type oauthStartResponse struct {
	Status    string `json:"status"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Flow      string `json:"flow"`
	ExpiresAt string `json:"expires_at"`
}

func TestRequestAnthropicTokenCodeModeReportsCodeFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousStore := oauthSessions
	oauthSessions = newOAuthSessionStore(oauthCallbackWaitTimeout)
	t.Cleanup(func() {
		oauthSessions = previousStore
	})

	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir(), Port: 8317}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/anthropic-auth-url?is_webui=true&callback_mode=code", nil)

	before := time.Now()
	h.RequestAnthropicToken(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload oauthStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	t.Cleanup(func() { SetOAuthSessionError(payload.State, "test shutdown") })

	if payload.Status != "ok" || payload.State == "" {
		t.Fatalf("payload = %+v, want status ok with a state", payload)
	}
	if payload.Flow != oauthFlowCode {
		t.Fatalf("flow = %q, want %q", payload.Flow, oauthFlowCode)
	}
	authURL, err := url.Parse(payload.URL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got := authURL.Query().Get("redirect_uri"); got != internalclaude.PlatformRedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", got, internalclaude.PlatformRedirectURI)
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", payload.ExpiresAt, err)
	}
	// RFC3339 drops sub-second precision, so allow a second on the low side.
	if earliest := before.Add(oauthSessionTTL - time.Second); expiresAt.Before(earliest) {
		t.Fatalf("expires_at = %s, want no earlier than %s", expiresAt, earliest)
	}
	if latest := time.Now().Add(oauthSessionTTL); expiresAt.After(latest) {
		t.Fatalf("expires_at = %s, want no later than %s", expiresAt, latest)
	}
}

func TestRequestCodexTokenReportsRedirectFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousStore := oauthSessions
	oauthSessions = newOAuthSessionStore(oauthCallbackWaitTimeout)
	t.Cleanup(func() {
		oauthSessions = previousStore
	})

	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir(), Port: 8317}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/codex-auth-url?is_webui=true", nil)

	h.RequestCodexToken(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload oauthStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	t.Cleanup(func() { SetOAuthSessionError(payload.State, "test shutdown") })

	if payload.Flow != oauthFlowRedirect {
		t.Fatalf("flow = %q, want %q", payload.Flow, oauthFlowRedirect)
	}
	if _, err := time.Parse(time.RFC3339, payload.ExpiresAt); err != nil {
		t.Fatalf("parse expires_at %q: %v", payload.ExpiresAt, err)
	}
}

func TestAnthropicLoginFlowFollowsIssuedRedirect(t *testing.T) {
	platform := "https://claude.ai/oauth/authorize?redirect_uri=" + url.QueryEscape(internalclaude.PlatformRedirectURI) + "&state=s"
	local := "https://claude.ai/oauth/authorize?redirect_uri=" + url.QueryEscape("http://localhost:54545/callback") + "&state=s"

	cases := map[string]string{
		platform:      oauthFlowCode,
		local:         oauthFlowRedirect,
		"":            oauthFlowRedirect,
		"://bad-url%": oauthFlowRedirect,
	}
	for authURL, want := range cases {
		if got := anthropicLoginFlow(authURL); got != want {
			t.Errorf("anthropicLoginFlow(%q) = %q, want %q", authURL, got, want)
		}
	}
}

func TestDeviceLoginExpiryTakesTheEarlierDeadline(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	sessionDeadline := now.Add(oauthSessionTTL)

	if got := deviceLoginExpiry(now, 0); !got.Equal(sessionDeadline) {
		t.Fatalf("no device lifetime: got %s, want session deadline %s", got, sessionDeadline)
	}
	short := int((oauthSessionTTL / 2).Seconds())
	if got, want := deviceLoginExpiry(now, short), now.Add(time.Duration(short)*time.Second); !got.Equal(want) {
		t.Fatalf("short device lifetime: got %s, want %s", got, want)
	}
	long := int((oauthSessionTTL * 3).Seconds())
	if got := deviceLoginExpiry(now, long); !got.Equal(sessionDeadline) {
		t.Fatalf("long device lifetime: got %s, want session deadline %s", got, sessionDeadline)
	}
}

func TestOAuthStartPayloadKeepsLegacyFieldsAndAddsDeviceDetails(t *testing.T) {
	expiresAt := time.Date(2026, 10, 5, 12, 10, 0, 0, time.FixedZone("UTC+8", 8*3600))
	payload := oauthStartPayload(oauthFlowDevice, "https://example.test/device?user_code=ABCD-1234", "state-1", expiresAt,
		deviceLoginExtra(" ABCD-1234 ", "https://example.test/device", 600))

	want := gin.H{
		"status":           "ok",
		"url":              "https://example.test/device?user_code=ABCD-1234",
		"state":            "state-1",
		"flow":             oauthFlowDevice,
		"expires_at":       "2026-10-05T04:10:00Z",
		"user_code":        "ABCD-1234",
		"verification_uri": "https://example.test/device",
		"expires_in":       600,
	}
	if len(payload) != len(want) {
		t.Fatalf("payload = %v, want %v", payload, want)
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("payload[%q] = %v, want %v", key, payload[key], value)
		}
	}

	if extra := deviceLoginExtra("", " ", 0); len(extra) != 0 {
		t.Fatalf("deviceLoginExtra with nothing to report = %v, want empty", extra)
	}
}
