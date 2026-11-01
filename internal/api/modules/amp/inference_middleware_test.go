package amp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// stage records the order middleware ran in and ends the request, so the real
// provider handlers (which need a configured server) never run.
func stage(order *[]string, name string, finish bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		*order = append(*order, name)
		if finish {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// The provider routes of the Amp module call the same handlers as /v1, which
// read the loop protection a middleware publishes. Without the middleware here a
// tenant that switched loop protection on would be protected on /v1 and not on
// these routes, with nothing to say so.
func TestProviderAliasesRunTheInferenceMiddlewareAfterAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/provider/openai/v1/chat/completions"},
		{http.MethodPost, "/api/provider/openai/chat/completions"},
		{http.MethodPost, "/api/provider/openai/v1/responses"},
		{http.MethodPost, "/api/provider/anthropic/v1/messages"},
		{http.MethodPost, "/api/provider/google/v1beta/models/gemini:generateContent"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var order []string
			m := &AmpModule{inferenceMiddleware: stage(&order, "inference", true)}
			r := gin.New()
			m.registerProviderAliases(r, &handlers.BaseAPIHandler{}, stage(&order, "auth", false))

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want the middleware's 204; the route did not run it", rec.Code)
			}
			if len(order) != 2 || order[0] != "auth" || order[1] != "inference" {
				t.Fatalf("order = %v, want auth then inference", order)
			}
		})
	}
}

func TestProviderAliasesDoNotRunTheInferenceMiddlewareForARefusedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var order []string
	m := &AmpModule{inferenceMiddleware: stage(&order, "inference", true)}
	r := gin.New()
	refuse := func(c *gin.Context) {
		order = append(order, "auth")
		c.AbortWithStatus(http.StatusUnauthorized)
	}
	m.registerProviderAliases(r, &handlers.BaseAPIHandler{}, refuse)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/openai/v1/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized || len(order) != 1 {
		t.Fatalf("status %d, order %v: a request authentication refused reached the middleware", rec.Code, order)
	}
}

func TestProviderAliasesWithoutAnInferenceMiddlewareAreUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	m := &AmpModule{}
	m.registerProviderAliases(r, &handlers.BaseAPIHandler{}, func(c *gin.Context) { c.AbortWithStatus(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/openai/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the auth stand-in's 200", rec.Code)
	}
}

// The Gemini bridge lives in the management group, which otherwise only proxies
// to the Amp control plane. Only that route gets the middleware.
func TestGoogleBridgeRunsTheInferenceMiddlewareAndTheProxiedRoutesDoNot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	proxy, err := createReverseProxy(upstream.URL, NewStaticSecretSource(""))
	if err != nil {
		t.Fatalf("createReverseProxy: %v", err)
	}

	var order []string
	m := &AmpModule{inferenceMiddleware: stage(&order, "inference", true)}
	m.setProxy(proxy)
	r := gin.New()
	m.registerManagementRoutes(r, &handlers.BaseAPIHandler{}, stage(&order, "auth", false))
	// A real server: the reverse proxy needs a response writer that can notify
	// of a closed connection, which a recorder cannot.
	srv := httptest.NewServer(r)
	defer srv.Close()

	do := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(http.MethodPost, "/api/provider/google/v1beta1/models/gemini:generateContent"); code != http.StatusNoContent || len(order) != 2 || order[1] != "inference" {
		t.Fatalf("bridge: status %d, order %v", code, order)
	}

	order = nil
	if code := do(http.MethodGet, "/api/threads"); code != http.StatusOK {
		t.Fatalf("proxied route status = %d", code)
	}
	for _, name := range order {
		if name == "inference" {
			t.Fatalf("a route that only proxies ran the inference middleware: %v", order)
		}
	}
}
