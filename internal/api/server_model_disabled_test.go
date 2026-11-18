package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Disabling a model in the catalog only ever stopped a config row from
// contributing a model of its own. Anything the static registry already served
// stayed listed and stayed callable, so "disabled" meant nothing for exactly
// the models an operator is most likely to switch off.

func initDisabledModelTestDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func seedModelConfig(t *testing.T, modelID string, enabled bool) {
	t.Helper()
	if _, err := modelconfigsettings.UpsertConfig(modelconfigsettings.UpsertConfigInput{
		ModelID:     modelID,
		Scope:       "active",
		OwnedBy:     "anthropic",
		Enabled:     enabled,
		PricingMode: "call",
	}); err != nil {
		t.Fatalf("UpsertConfig(%q) error = %v", modelID, err)
	}
}

// runRestrictionMiddleware drives just the middleware, so the assertions are
// about the disabled-model decision and not about routing or upstream calls.
func runRestrictionMiddleware(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := &Server{cfg: &config.Config{}}
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.modelRestrictionMiddleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"reached_upstream": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// runGeminiRestrictionMiddleware drives the middleware mounted on the
// Gemini-native /models/*action route, where the model is carried in the URL
// and the body has no "model" field.
func runGeminiRestrictionMiddleware(t *testing.T, model, action string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := &Server{cfg: &config.Config{}}
	engine := gin.New()
	engine.POST("/v1beta/models/*action", server.modelRestrictionMiddleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"reached_upstream": true})
	})

	path := fmt.Sprintf("/v1beta/models/%s:%s", model, action)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"contents":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestDisabledModelGeminiNativeURLEndpointIsRejected(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "gemini-2.5-flash", false)

	rec := runGeminiRestrictionMiddleware(t, "gemini-2.5-flash", "generateContent")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := gjson.Get(rec.Body.String(), "error.code").String(); code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", code)
	}
	if strings.Contains(rec.Body.String(), "reached_upstream") {
		t.Error("Gemini-native request reached upstream despite model being disabled")
	}
}

func TestEnabledModelGeminiNativeURLEndpointPasses(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "gemini-2.5-flash", true)

	rec := runGeminiRestrictionMiddleware(t, "gemini-2.5-flash", "generateContent")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reached_upstream") {
		t.Error("expected request to reach upstream")
	}
}

func TestDisabledModelRequestIsRejected(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)

	rec := runRestrictionMiddleware(t, `{"model":"claude-opus-4-6"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if code := gjson.Get(rec.Body.String(), "error.code").String(); code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", code)
	}
	if strings.Contains(rec.Body.String(), "reached_upstream") {
		t.Error("request reached the upstream handler despite the model being disabled")
	}
}

func TestEnabledModelRequestPassesThrough(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)
	seedModelConfig(t, "claude-opus-5", true)

	rec := runRestrictionMiddleware(t, `{"model":"claude-opus-5"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reached_upstream") {
		t.Error("enabled model did not reach the upstream handler")
	}
}

func TestModelWithNoConfigRowPassesThrough(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)

	// A model the operator never touched has no row at all; it must not be
	// swept up by the disabled check.
	rec := runRestrictionMiddleware(t, `{"model":"gpt-5"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestDisabledModelCheckIsCaseInsensitive(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)

	rec := runRestrictionMiddleware(t, `{"model":"CLAUDE-Opus-4-6"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestPrefixedAliasOfDisabledModelIsRejected(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "gpt-oss:20b", false)

	// Provider-prefixed aliases route to the bare ID, so they must not be a way
	// around the toggle.
	rec := runRestrictionMiddleware(t, `{"model":"ollama/gpt-oss:20b"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestRequestWithoutModelIsUntouched(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)

	rec := runRestrictionMiddleware(t, `{}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestDropDisabledCatalogModelsFiltersListing(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-4-6", false)
	seedModelConfig(t, "claude-opus-5", true)

	models := []map[string]interface{}{
		{"id": "claude-opus-4-6"},
		{"id": "claude-opus-5"},
		{"id": "gpt-5"},
	}

	got := modelconfigsettings.FilterOutDisabled("", models)

	var ids []string
	for _, model := range got {
		id, _ := model["id"].(string)
		ids = append(ids, id)
	}
	want := []string{"claude-opus-5", "gpt-5"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestDropDisabledCatalogModelsKeepsListWhenNothingDisabled(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "claude-opus-5", true)

	models := []map[string]interface{}{{"id": "claude-opus-5"}, {"id": "gpt-5"}}
	if got := modelconfigsettings.FilterOutDisabled("", models); len(got) != 2 {
		encoded, _ := json.Marshal(got)
		t.Fatalf("FilterOutDisabled() = %s, want both models", encoded)
	}
}

// The pi CLIProxyAPI provider (and the Codex CLI) drive /responses over a
// WebSocket. The upgrade is a GET with no body, so the middleware never saw the
// model, and every turn's model — sent inside a frame — went straight to the
// executor. Disabling a model in the catalog therefore hid it from /v1/models
// but left it fully callable from pi. The middleware now publishes its gate on
// the gin context and the frame loop applies it per turn.
func dialResponsesWebsocket(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, coreauth.NewManager(nil, nil, nil))
	server := &Server{cfg: cfg, handlers: base}
	responses := openai.NewOpenAIResponsesAPIHandler(base)

	engine := gin.New()
	engine.GET("/v1/responses", server.modelRestrictionMiddleware(), responses.ResponsesWebsocket)
	httpServer := httptest.NewServer(engine)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		httpServer.Close()
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	return conn, func() { _ = conn.Close(); httpServer.Close() }
}

func TestDisabledModelIsRefusedOnResponsesWebsocket(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "gemini-2.5-flash", false)

	conn, closeAll := dialResponsesWebsocket(t)
	defer closeAll()

	frame := `{"type":"response.create","model":"gemini-2.5-flash","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, reply, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}

	body := string(reply)
	if typ := gjson.Get(body, "type").String(); typ != "error" {
		t.Fatalf("reply type = %q, want error (payload %s)", typ, body)
	}
	if status := gjson.Get(body, "status").Int(); status != http.StatusNotFound {
		t.Fatalf("reply status = %d, want 404 (payload %s)", status, body)
	}
	if !strings.Contains(body, "is disabled") {
		t.Fatalf("reply = %s, want the disabled-model message", body)
	}
}

func TestEnabledModelPassesGateOnResponsesWebsocket(t *testing.T) {
	initDisabledModelTestDB(t)
	seedModelConfig(t, "gemini-2.5-flash", false)
	seedModelConfig(t, "gemini-3.8-flash-high", true)

	conn, closeAll := dialResponsesWebsocket(t)
	defer closeAll()

	frame := `{"type":"response.create","model":"gemini-3.8-flash-high","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, reply, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	// With no upstream credentials the executor fails, but that failure must be
	// the executor's, not the gate's: the model reached execution.
	body := string(reply)
	if strings.Contains(body, "is disabled") || gjson.Get(body, "status").Int() == http.StatusNotFound {
		t.Fatalf("enabled model was refused by the gate: %s", body)
	}
}
