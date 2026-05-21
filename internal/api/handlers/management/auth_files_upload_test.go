package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type recordingPersistAuthStore struct {
	memoryAuthStore
	persistedPaths []string
}

func (s *recordingPersistAuthStore) PersistAuthFiles(ctx context.Context, message string, paths ...string) error {
	_ = ctx
	_ = message
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistedPaths = append(s.persistedPaths, paths...)
	return nil
}

func TestUploadAuthFileRejectsOversizedMultipart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
		authManager: manager,
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "oversized.json")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	payload := bytes.Repeat([]byte("a"), int(bodyutil.AuthFileBodyLimit)+1)
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("Write payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/auth-files", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.Request = req

	h.UploadAuthFile(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}

	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files written, got %d", len(entries))
	}
}

func TestUploadAuthFilePersistsUploadedJSONThroughStorePersister(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := &recordingPersistAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
		authManager: manager,
		tokenStore:  store,
	}

	payload := []byte(`{"type":"codex","email":"subscriber@example.com","subscription_started_at":"2027-01-02T03:04:00Z","subscription_period":"monthly"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/auth-files?name=codex-subscription.json", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.UploadAuthFile(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	wantPath := filepath.Join(authDir, "codex-subscription.json")
	store.mu.Lock()
	gotPaths := append([]string(nil), store.persistedPaths...)
	store.mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != wantPath {
		t.Fatalf("persisted paths = %#v, want [%q]", gotPaths, wantPath)
	}
}

func TestRegisterAuthFromFileAppliesRoutingMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
		authManager: manager,
	}

	fileName := "claude-pro.json"
	absPath := filepath.Join(authDir, fileName)
	data := []byte(`{"type":"claude","email":"pro@example.com","prefix":"team-a","proxy_url":"http://auth-proxy.local:8080","proxy_id":"premium-egress"}`)
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), absPath, data); err != nil {
		t.Fatalf("registerAuthFromFile: %v", err)
	}

	auth, ok := manager.GetByID(fileName)
	if !ok || auth == nil {
		t.Fatalf("registered auth not found")
	}
	if auth.Prefix != "team-a" {
		t.Fatalf("Prefix = %q, want team-a", auth.Prefix)
	}
	if auth.ProxyURL != "http://auth-proxy.local:8080" {
		t.Fatalf("ProxyURL = %q, want auth proxy", auth.ProxyURL)
	}
	if auth.ProxyID != "premium-egress" {
		t.Fatalf("ProxyID = %q, want premium-egress", auth.ProxyID)
	}
}

func TestRegisterAuthFromFileInfersCodexProviderForOpenAIOAuthJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
		authManager: manager,
	}

	fileName := "openai-oauth.json"
	absPath := filepath.Join(authDir, fileName)
	data := []byte(`{"chatgpt_account_id":"acct-123","client_id":"app_test","access_token":"access-token","id_token":"id-token","email":"subscriber@example.com","plan_type":"plus"}`)
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), absPath, data); err != nil {
		t.Fatalf("registerAuthFromFile: %v", err)
	}

	auth, ok := manager.GetByID(fileName)
	if !ok || auth == nil {
		t.Fatalf("registered auth not found")
	}
	if auth.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", auth.Provider)
	}
	if auth.Metadata["type"] != "codex" {
		t.Fatalf("metadata type = %#v, want codex", auth.Metadata["type"])
	}
}

func TestRegisterAuthFromFileNormalizesOpenAIBundleJSONForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
		authManager: manager,
	}

	accountID := "acct-bundle"
	issuedAt := int64(1_779_210_280)
	expiresAt := int64(1_780_074_280)
	accessToken := makeJWTForAuthFilesUploadTest(t, map[string]any{
		"iat": issuedAt,
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	idToken := makeJWTForAuthFilesUploadTest(t, map[string]any{
		"email": "bundle@example.com",
		"iat":   issuedAt,
		"exp":   expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	fileName := "openai-bundle.json"
	absPath := filepath.Join(authDir, fileName)
	data, err := json.Marshal(map[string]any{
		"version":              1,
		"platform":             "openai",
		"account_claims_email": "bundle@example.com",
		"access_token":         accessToken,
		"id_token":             idToken,
		"refresh_token":        "",
		"client_id":            "app_test",
		"chatgpt_account_id":   accountID,
		"disabled":             false,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), absPath, data); err != nil {
		t.Fatalf("registerAuthFromFile: %v", err)
	}

	auth, ok := manager.GetByID(fileName)
	if !ok || auth == nil {
		t.Fatalf("registered auth not found")
	}
	if auth.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", auth.Provider)
	}
	wantExpired := time.Unix(expiresAt, 0).UTC().Format(time.RFC3339)
	wantLastRefresh := time.Unix(issuedAt, 0).UTC().Format(time.RFC3339)
	for key, want := range map[string]any{
		"type":         "codex",
		"account_id":   accountID,
		"email":        "bundle@example.com",
		"expired":      wantExpired,
		"last_refresh": wantLastRefresh,
		"plan_type":    "plus",
	} {
		if got := auth.Metadata[key]; got != want {
			t.Fatalf("metadata[%s] = %#v, want %#v", key, got, want)
		}
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("Unmarshal persisted: %v", err)
	}
	if persisted["account_id"] != accountID || persisted["type"] != "codex" {
		t.Fatalf("persisted normalized fields = %#v", persisted)
	}
}

func makeJWTForAuthFilesUploadTest(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}

func TestImportVertexCredentialRejectsOversizedMultipart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := &Handler{
		cfg: &config.Config{
			AuthDir: authDir,
		},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "vertex.json")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	payload := bytes.Repeat([]byte("a"), int(bodyutil.VertexCredentialBodyLimit)+1)
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("Write payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/vertex/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.Request = req

	h.ImportVertexCredential(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(authDir, "vertex.json")); err == nil {
		t.Fatal("unexpected credential file written")
	}
}

func TestRegisterAuthFromFileUsesRelativeIDForRelativeAuthDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rootDir := t.TempDir()
	authDirAbs := filepath.Join(rootDir, "auths")
	if err := os.MkdirAll(authDirAbs, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(rootDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(previousWD)
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg: &config.Config{
			AuthDir: "auths",
		},
		authManager: manager,
	}

	fileName := "codex-test.json"
	absPath := filepath.Join(authDirAbs, fileName)
	data := []byte(`{"type":"codex","email":"test@example.com"}`)
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := h.authIDForPath(absPath); got != fileName {
		t.Fatalf("authIDForPath(%q) = %q, want %q", absPath, got, fileName)
	}

	watcherID := fileName
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       watcherID,
		FileName: fileName,
		Provider: "codex",
		Label:    "test@example.com",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": absPath,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Register existing auth: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), absPath, data); err != nil {
		t.Fatalf("registerAuthFromFile: %v", err)
	}

	auths := manager.List()
	if len(auths) != 1 {
		ids := make([]string, 0, len(auths))
		for _, auth := range auths {
			ids = append(ids, auth.ID)
		}
		t.Fatalf("auth count = %d, want 1 (ids=%v)", len(auths), ids)
	}
	if auths[0].ID != watcherID {
		t.Fatalf("auth id = %q, want %q", auths[0].ID, watcherID)
	}
	if _, ok := manager.GetByID(absPath); ok {
		t.Fatalf("unexpected duplicate auth registered with absolute path id %q", absPath)
	}
}
