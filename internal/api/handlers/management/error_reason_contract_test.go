package management

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// These handlers answer a failure as {"error": "<code>", "message": "<reason>"},
// a shape inherited from upstream that older panels and outside scripts read.
// The panel shows it as "code: reason" (codeProxy extractApiErrorMessage), so
// these tests pin what that relies on: the code stays as it is and the message
// names the actual cause (the YAML line, the channel group, the path) rather
// than restating the code.

type flatErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func requireFlatError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, cause string) {
	t.Helper()
	var body flatErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, rec.Body.String())
	}
	if rec.Code != status || body.Error != code {
		t.Fatalf("status = %d, error = %q, want %d %q; body=%s", rec.Code, body.Error, status, code, rec.Body.String())
	}
	if !strings.Contains(body.Message, cause) {
		t.Fatalf("message = %q, want it to name the cause %q", body.Message, cause)
	}
}

func putConfigYAMLForTest(t *testing.T, configPath string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", bytes.NewReader(body))
	NewHandler(&config.Config{}, configPath, nil).PutConfigYAML(c)
	return rec
}

func TestPutConfigYAMLErrorsNameTheCause(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// With a config store open, a save that changes the routing section is
	// answered by the database-owned section check before the channel groups
	// are validated; without one it reaches that validation.
	usage.CloseDB()

	t.Run("YAML the parser rejects", func(t *testing.T) {
		rec := putConfigYAMLForTest(t, filepath.Join(t.TempDir(), "config.yaml"), []byte("port: 8318\nrouting: [\n"))
		requireFlatError(t, rec, http.StatusBadRequest, "invalid_yaml", "yaml: line ")
	})

	t.Run("a channel group declared twice", func(t *testing.T) {
		body := []byte("routing:\n  channel-groups:\n    - name: team-a\n    - name: team-a\n")
		rec := putConfigYAMLForTest(t, filepath.Join(t.TempDir(), "config.yaml"), body)
		requireFlatError(t, rec, http.StatusUnprocessableEntity, "invalid_config", `duplicate channel group "team-a"`)
	})

	t.Run("a config directory that is gone", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		rec := putConfigYAMLForTest(t, filepath.Join(dir, "config.yaml"), []byte("port: 8318\n"))
		requireFlatError(t, rec, http.StatusInternalServerError, "write_failed", dir)
	})

	t.Run("a body over the limit", func(t *testing.T) {
		body := bytes.Repeat([]byte("a"), int(bodyutil.ConfigYAMLBodyLimit)+1)
		rec := putConfigYAMLForTest(t, filepath.Join(t.TempDir(), "config.yaml"), body)
		requireFlatError(t, rec, http.StatusRequestEntityTooLarge, "request_body_too_large", "exceeds limit")
	})
}

func TestGetConfigYAMLMissingFileNamesTheCause(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil).GetConfigYAML(c)

	requireFlatError(t, rec, http.StatusNotFound, "not_found", "config file not found")
}

func TestPutRequestLogBodyStorageConfirmationNamesTheMissingField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/request-log-storage/store-content", bytes.NewBufferString(`{"value":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil).PutRequestLogBodyStorage(c)

	requireFlatError(t, rec, http.StatusBadRequest, "confirmation_required", "clear_existing must be true")
}

func TestImportVertexCredentialErrorsNameTheCause(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name  string
		file  string
		code  string
		cause string
	}{
		// The Vertex import's codes are phrases; the panel shows them as they are.
		{name: "a file that is not JSON", file: "{not json", code: "invalid json", cause: "invalid character"},
		{name: "a service account without a private key", file: `{"type":"service_account","project_id":"demo"}`, code: "invalid service account", cause: "missing private_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "vertex.json")
			if err != nil {
				t.Fatalf("CreateFormFile: %v", err)
			}
			if _, err := part.Write([]byte(tt.file)); err != nil {
				t.Fatalf("write file part: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close multipart writer: %v", err)
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/vertex/import", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			(&Handler{cfg: &config.Config{AuthDir: t.TempDir()}}).ImportVertexCredential(c)

			requireFlatError(t, rec, http.StatusBadRequest, tt.code, tt.cause)
		})
	}
}
