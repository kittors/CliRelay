package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// fakeAntigravityGoogle answers the token endpoint and loadCodeAssist in
// process, so refresh and discovery run end to end without reaching Google.
type fakeAntigravityGoogle struct {
	load  string
	loads atomic.Int32
}

func (f *fakeAntigravityGoogle) context(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), util.ContextKeyRoundTripper, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := ""
		switch req.URL.String() {
		case "https://oauth2.googleapis.com/token":
			body = `{"access_token":"fresh-token","expires_in":3599,"token_type":"Bearer"}`
		case antigravityauth.APIEndpoint + "/" + antigravityauth.APIVersion + ":loadCodeAssist":
			f.loads.Add(1)
			body = f.load
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return nil, fmt.Errorf("unexpected request %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}))
}

func discoveryTestExecutor() *AntigravityExecutor {
	return &AntigravityExecutor{cfg: &config.Config{
		OAuthClients: config.OAuthClients{
			Antigravity: config.OAuthClient{ClientID: "client-id", ClientSecret: "client-secret"},
		},
	}}
}

// A credential production still carries: no project, and the September
// "no project" flag written while discovery used the wrong client. The
// background refresh must ask again and keep what it finds (#1032).
func TestAntigravityRefreshRecoversProjectBehindExpiredVerdict(t *testing.T) {
	google := &fakeAntigravityGoogle{load: `{"cloudaicompanionProject":"real-project","currentTier":{"id":"free-tier"}}`}
	auth := &cliproxyauth.Auth{ID: "antigravity-a.json", Metadata: map[string]any{
		"refresh_token":                          "refresh-token",
		antigravityauth.MetaProjectIDUnavailable: true,
		antigravityauth.MetaProjectIDProbedAt:    float64(time.Date(2026, 9, 16, 9, 22, 0, 0, time.UTC).UnixMilli()),
		antigravityauth.MetaProjectIDProbeErr:    "no project_id in response",
	}}

	updated, err := discoveryTestExecutor().Refresh(google.context(t), auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := metaStringValue(updated.Metadata, "project_id"); got != "real-project" {
		t.Fatalf("project_id = %q, want real-project", got)
	}
	for _, key := range []string{antigravityauth.MetaProjectIDUnavailable, antigravityauth.MetaProjectIDProbedAt, antigravityauth.MetaProjectIDProbeErr} {
		if _, ok := updated.Metadata[key]; ok {
			t.Fatalf("%s survived a successful discovery", key)
		}
	}
	if got := google.loads.Load(); got != 1 {
		t.Fatalf("loadCodeAssist calls = %d, want 1", got)
	}
	if _, ok := auth.Metadata["project_id"]; ok {
		t.Fatal("Refresh must work on a copy, not the caller's credential")
	}
}

// A refusal is recorded so the next refreshes leave the account alone for the
// verdict window, and the refreshed token is still returned.
func TestAntigravityRefreshRecordsRefusedDiscovery(t *testing.T) {
	google := &fakeAntigravityGoogle{load: `{"ineligibleTiers":[{"reasonCode":"UNSUPPORTED_CLIENT","reasonMessage":"unsupported"}]}`}
	auth := &cliproxyauth.Auth{ID: "antigravity-b.json", Metadata: map[string]any{"refresh_token": "refresh-token"}}
	before := time.Now()

	updated, err := discoveryTestExecutor().Refresh(google.context(t), auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := metaStringValue(updated.Metadata, "access_token"); got != "fresh-token" {
		t.Fatalf("access_token = %q, want the refreshed token despite the failed discovery", got)
	}
	if updated.Metadata[antigravityauth.MetaProjectIDUnavailable] != true {
		t.Fatalf("unavailable = %#v, want true", updated.Metadata[antigravityauth.MetaProjectIDUnavailable])
	}
	if !strings.Contains(metaStringValue(updated.Metadata, antigravityauth.MetaProjectIDProbeErr), "UNSUPPORTED_CLIENT") {
		t.Fatalf("probe_err = %q, want the refusal", metaStringValue(updated.Metadata, antigravityauth.MetaProjectIDProbeErr))
	}
	if !antigravityauth.ShouldSkipProjectIDProbe(updated.Metadata, before.Add(time.Hour), 0) {
		t.Fatal("the recorded refusal should hold off the next probe")
	}
}

// Requests refresh tokens on their own copy of the credential, which is
// dropped afterwards. Discovery there could not be saved or backed off, so
// the request path must leave it to the background refresh.
func TestAntigravityRequestPathRefreshDoesNotDiscoverProject(t *testing.T) {
	google := &fakeAntigravityGoogle{load: `{"cloudaicompanionProject":"real-project"}`}
	auth := &cliproxyauth.Auth{ID: "antigravity-c.json", Metadata: map[string]any{"refresh_token": "refresh-token"}}

	token, _, err := discoveryTestExecutor().ensureAccessToken(google.context(t), auth)
	if err != nil {
		t.Fatalf("ensureAccessToken: %v", err)
	}
	if token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", token)
	}
	if got := google.loads.Load(); got != 0 {
		t.Fatalf("loadCodeAssist calls on the request path = %d, want 0", got)
	}
}
