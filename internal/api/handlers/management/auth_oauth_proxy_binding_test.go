package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The login dialog offers an "authorization proxy" and the panel sends it as
// proxy_id, but nothing on this side used to read it: the record was written
// with an empty ProxyID, so every probe and API call for that account went out
// through the deployment host's own address. On an Antigravity account that
// showed up as a paid plan being reported back as restricted, because the tier
// lookup is answered per egress IP.
func TestOAuthSaveRecordBindsSelectedProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/antigravity-auth-url?is_webui=true&proxy_id=us-residential", nil)

	_, saveRecord, _, _ := h.tenantOAuthBindings(c)

	record := &coreauth.Auth{
		Provider: "antigravity",
		FileName: "antigravity-someone@example.com.json",
		Metadata: map[string]any{"email": "someone@example.com"},
	}
	if _, err := saveRecord(context.Background(), record); err != nil {
		t.Fatalf("saveRecord() error = %v", err)
	}

	if record.ProxyID != "us-residential" {
		t.Fatalf("ProxyID = %q, want the proxy chosen in the login dialog", record.ProxyID)
	}
	// Runtime egress reads the field; a reload rebuilds it from metadata. A
	// binding that is missing from either one silently stops applying.
	if got := record.Metadata["proxy_id"]; got != "us-residential" {
		t.Fatalf("metadata proxy_id = %v, want the binding to survive a reload", got)
	}
}

func TestOAuthSaveRecordLeavesProxyUnsetWhenDialogPickedNone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/antigravity-auth-url?is_webui=true", nil)

	_, saveRecord, _, _ := h.tenantOAuthBindings(c)

	record := &coreauth.Auth{
		Provider: "antigravity",
		FileName: "antigravity-someone@example.com.json",
		Metadata: map[string]any{"email": "someone@example.com"},
	}
	if _, err := saveRecord(context.Background(), record); err != nil {
		t.Fatalf("saveRecord() error = %v", err)
	}

	if record.ProxyID != "" {
		t.Fatalf("ProxyID = %q, want empty when the dialog left the proxy blank", record.ProxyID)
	}
	if _, exists := record.Metadata["proxy_id"]; exists {
		t.Fatal("an unset proxy must not be written to metadata")
	}
}
