package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	codexprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/codex"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// clusterCodexOAuth stands in for OpenAI. The token exchange only succeeds
// with the verifier generated for the login, which only node A holds.
type clusterCodexOAuth struct{}

func (clusterCodexOAuth) GenerateAuthURL(state string, _ *internalcodex.PKCECodes) (string, error) {
	return "https://auth.example.com/authorize?state=" + state, nil
}

func (clusterCodexOAuth) ExchangeCodeForTokens(_ context.Context, code string, pkce *internalcodex.PKCECodes) (*internalcodex.CodexAuthBundle, error) {
	if code != "auth-code" || pkce == nil || pkce.CodeVerifier != "node-a-verifier" {
		return nil, errors.New("exchange must use node A's verifier and the delivered code")
	}
	return &internalcodex.CodexAuthBundle{TokenData: internalcodex.CodexTokenData{AccessToken: "access"}}, nil
}

func (clusterCodexOAuth) CreateTokenStorage(bundle *internalcodex.CodexAuthBundle) *internalcodex.CodexTokenStorage {
	return &internalcodex.CodexTokenStorage{AccessToken: bundle.TokenData.AccessToken, Email: "cluster@example.com", Type: "codex"}
}

func clusterOAuthNodes(t *testing.T) (nodeA, nodeB *oauthsession.ClusterStore, repo *oauthsession.MemoryRepo) {
	t.Helper()
	hub := cluster.NewMemoryHub()
	coordA, coordB := hub.Join("node-a"), hub.Join("node-b")
	repo = oauthsession.NewMemoryRepo()
	nodeA = oauthsession.NewClusterStore(repo, func() *cluster.Coordinator { return coordA }, time.Minute)
	nodeB = oauthsession.NewClusterStore(repo, func() *cluster.Coordinator { return coordB }, time.Minute)
	// Polling is effectively off: node A must be woken by node B's event.
	nodeA.SetPollInterval(time.Hour)
	t.Cleanup(nodeA.Close)
	t.Cleanup(nodeB.Close)
	// This process serves node B's HTTP side.
	SetSharedOAuthSessions(nodeB)
	t.Cleanup(func() { SetSharedOAuthSessions(nil) })
	return nodeA, nodeB, repo
}

func getAuthStatus(t *testing.T, h *Handler, state string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/get-auth-status?state="+state, nil)
	h.GetAuthStatus(c)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func postOAuthCallback(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-callback", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	h.PostOAuthCallback(c)
	return rec
}

// Node A starts a Codex login, the pasted callback reaches node B, node A
// finishes the login with its own verifier, and node B reports success.
func TestClusterOAuthCallbackOnAnotherNodeCompletesLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	nodeA, _, _ := clusterOAuthNodes(t)
	handlerB := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}

	saved := make(chan *coreauth.Auth, 1)
	result, err := codexprovider.StartOAuthLogin(context.Background(), codexprovider.OAuthLoginOptions{
		Auth:                clusterCodexOAuth{},
		CallbackWaitTimeout: 5 * time.Second,
		GeneratePKCE: func() (*internalcodex.PKCECodes, error) {
			return &internalcodex.PKCECodes{CodeVerifier: "node-a-verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "cluster-codex-state", nil },
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			return nodeA.WaitCallback(authDir, provider, state, timeout, 0)
		},
		SaveRecord: func(_ context.Context, record *coreauth.Auth) (string, error) {
			saved <- record
			return "stored", nil
		},
		Sessions: codexprovider.SessionCallbacks{
			Register:         func(state, provider string) { nodeA.RegisterTenant(state, provider, "") },
			SetError:         nodeA.SetError,
			Complete:         nodeA.Complete,
			CompleteProvider: func(provider string) int { return nodeA.CompleteProviderTenant(provider, "") },
		},
	})
	if err != nil {
		t.Fatalf("start login on node A: %v", err)
	}

	if code, body := getAuthStatus(t, handlerB, result.State); code != http.StatusOK || body["status"] != "wait" {
		t.Fatalf("node B status before callback = %d %v", code, body)
	}
	rec := postOAuthCallback(t, handlerB, `{"provider":"codex","redirect_url":"http://localhost:1455/auth/callback?code=auth-code&state=`+result.State+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("node B callback status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case record := <-saved:
		if record == nil || record.Provider != "codex" {
			t.Fatalf("saved record = %+v", record)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("node A never finished the login")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, body := getAuthStatus(t, handlerB, result.State)
		if code == http.StatusOK && body["status"] == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node B status after login = %d %v", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Submitting the same callback again is harmless.
	again := postOAuthCallback(t, handlerB, `{"provider":"codex","code":"auth-code","state":"`+result.State+`"}`)
	if again.Code != http.StatusOK || !bytes.Contains(again.Body.Bytes(), []byte(`"already_processed":true`)) {
		t.Fatalf("repeated callback = %d %s", again.Code, again.Body.String())
	}
}

func TestClusterAuthStatusReportsUnknownExpiredSupersededAndFailedStates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	nodeA, _, repo := clusterOAuthNodes(t)
	handlerB := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}

	code, body := getAuthStatus(t, handlerB, "never-started")
	if code != http.StatusNotFound || body["status"] != "error" {
		t.Fatalf("unknown state = %d %v; a cluster must not report an unknown login as ok", code, body)
	}

	nodeA.RegisterTenant("superseded", "codex", "")
	nodeA.RegisterTenant("winner", "codex", "")
	nodeA.Complete("winner")
	nodeA.CompleteProviderTenant("codex", "")
	if code, body := getAuthStatus(t, handlerB, "superseded"); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("superseded state = %d %v, want ok as on a single node", code, body)
	}

	nodeA.RegisterTenant("failed", "xai", "")
	nodeA.SetError("failed", "Failed to exchange token")
	if code, body := getAuthStatus(t, handlerB, "failed"); code != http.StatusOK || body["error"] != "Failed to exchange token" {
		t.Fatalf("failed state = %d %v", code, body)
	}

	nodeA.RegisterTenant("expiring", "gemini", "")
	later := time.Now().Add(2 * time.Minute)
	repo.SetClock(func() time.Time { return later })
	if code, body := getAuthStatus(t, handlerB, "expiring"); code != http.StatusNotFound || body["error"] != oauthsession.MessageExpired || body["code"] != oauthCodeSessionExpired {
		t.Fatalf("expired state = %d %v", code, body)
	}
	if code, body := getAuthStatus(t, handlerB, "never-started"); body["code"] != oauthCodeSessionExpired {
		t.Fatalf("unknown state = %d %v, want code %q", code, body, oauthCodeSessionExpired)
	}
}

// The shared store hides cancelled rows from Get, so without a Lookup a late
// callback for a superseded login read as an unknown state. It now gets the
// same answer as on a single node, but only within its own tenant.
func TestClusterLateCallbackForSupersededLoginReportsSuperseded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	nodeA, _, _ := clusterOAuthNodes(t)
	handlerB := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}

	nodeA.RegisterTenant("loser", "codex", "")
	nodeA.RegisterTenant("winner", "codex", "")
	nodeA.Complete("winner")
	nodeA.CompleteProviderTenant("codex", "")

	rec := postOAuthCallback(t, handlerB, `{"provider":"codex","code":"c","state":"loser"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), oauthCodeSessionSuperseded) {
		t.Fatalf("late callback for superseded login = %d %s, want 409 %s", rec.Code, rec.Body.String(), oauthCodeSessionSuperseded)
	}

	nodeA.RegisterTenant("other-loser", "codex", "tenant-other")
	nodeA.RegisterTenant("other-winner", "codex", "tenant-other")
	nodeA.Complete("other-winner")
	nodeA.CompleteProviderTenant("codex", "tenant-other")
	rec = postOAuthCallback(t, handlerB, `{"provider":"codex","code":"c","state":"other-loser"}`)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), oauthCodeSessionSuperseded) {
		t.Fatalf("another tenant's superseded login = %d %s, want a plain 404", rec.Code, rec.Body.String())
	}
}

func TestClusterOAuthCallbackForUnknownStateIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, _, _ = clusterOAuthNodes(t)
	handlerB := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	rec := postOAuthCallback(t, handlerB, `{"provider":"codex","code":"c","state":"no-such-state"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("callback for unknown state = %d %s", rec.Code, rec.Body.String())
	}
}

// A single node used to answer "ok" for a state it had never seen, because
// it deleted superseded logins and could not tell them from expired ones. A
// superseded login now leaves a tombstone and still answers "ok"; a state the
// node has no record of is reported as expired, as the cluster store does.
func TestSingleNodeAuthStatusSeparatesSupersededFromExpired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })

	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	code, body := getAuthStatus(t, h, "never-started")
	if code != http.StatusNotFound || body["status"] != "error" || body["code"] != oauthCodeSessionExpired {
		t.Fatalf("single-node unknown state = %d %v, want 404 session_expired", code, body)
	}

	RegisterOAuthSession("finished", "codex")
	RegisterOAuthSession("replaced", "codex")
	CompleteOAuthSession("finished")
	if n := CompleteOAuthSessionsByProvider("codex"); n != 1 {
		t.Fatalf("superseded %d sessions, want 1", n)
	}
	if code, body := getAuthStatus(t, h, "replaced"); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("superseded login status = %d %v, want ok", code, body)
	}
	if code, body := getAuthStatus(t, h, "finished"); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("completed login status = %d %v, want ok", code, body)
	}

	// A late callback for the replaced login is turned away with a readable reason.
	rec := postOAuthCallback(t, h, `{"provider":"codex","code":"c","state":"replaced"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), oauthCodeSessionSuperseded) {
		t.Fatalf("callback for superseded login = %d %s", rec.Code, rec.Body.String())
	}
}
