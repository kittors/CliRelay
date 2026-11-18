package management

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
	wantPath := filepath.Join(authDir, identity.SystemTenantID, "codex-subscription.json")
	store.mu.Lock()
	gotPaths := append([]string(nil), store.persistedPaths...)
	store.mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != wantPath {
		t.Fatalf("persisted paths = %#v, want [%q]", gotPaths, wantPath)
	}
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
