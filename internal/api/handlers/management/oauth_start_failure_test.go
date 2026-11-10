package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// TestRequestXAITokenSaysWhyTheLoginCouldNotStart replays 2026-11-09: no proxy
// was picked in the login dialog, so the login left through the global
// proxy-url, and that proxy refused it. The answer has to carry the refusal,
// not only "failed to generate authorization url".
func TestRequestXAITokenSaysWhyTheLoginCouldNotStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousStore := oauthSessions
	oauthSessions = newOAuthSessionStore(oauthCallbackWaitTimeout)
	t.Cleanup(func() {
		oauthSessions = previousStore
	})

	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Host)
		mu.Unlock()
		http.Error(w, "refused by test proxy", http.StatusProxyAuthRequired)
	}))
	t.Cleanup(proxy.Close)

	h := &Handler{cfg: &config.Config{
		AuthDir:   t.TempDir(),
		Port:      8317,
		SDKConfig: config.SDKConfig{ProxyURL: proxy.URL},
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/xai-auth-url?is_webui=true", nil)

	h.RequestXAIToken(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Error != "failed to generate authorization url" {
		t.Fatalf("error = %q, want the existing code kept", body.Error)
	}
	for _, want := range []string{"auth.x.ai", "Proxy Authentication Required"} {
		if !strings.Contains(body.Message, want) {
			t.Fatalf("message = %q, want it to say %q", body.Message, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "auth.x.ai:443" {
		t.Fatalf("global proxy was asked for %v, want the discovery's CONNECT to auth.x.ai:443", asked)
	}
}

func TestOAuthStartFailureLeavesURLCredentialsOut(t *testing.T) {
	err := errors.New(`dial socks5://alice:s3cret@10.0.0.1:1080 then http://bob:pw@proxy.example:8080/x: refused`)
	body := oauthStartFailure("failed to start oauth flow", err)

	if body["error"] != "failed to start oauth flow" {
		t.Fatalf("error = %v, want the code", body["error"])
	}
	message, _ := body["message"].(string)
	for _, secret := range []string{"alice", "s3cret", "bob:pw"} {
		if strings.Contains(message, secret) {
			t.Fatalf("message %q leaks %q", message, secret)
		}
	}
	for _, kept := range []string{"socks5://***@10.0.0.1:1080", "http://***@proxy.example:8080/x", "refused"} {
		if !strings.Contains(message, kept) {
			t.Fatalf("message %q lost %q", message, kept)
		}
	}

	if _, ok := oauthStartFailure("failed to start oauth flow", nil)["message"]; ok {
		t.Fatalf("a failure without a cause got a message")
	}
}
