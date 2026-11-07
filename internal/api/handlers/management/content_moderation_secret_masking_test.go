package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func secretMaskingCall(h *Handler, method, tenantID, body string) (int, map[string]any, string) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, tenantPrincipal(tenantID))
	c.Request = httptest.NewRequest(method, "/v0/management/content-moderation/secret-masking", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		h.GetSecretMasking(c)
	} else {
		h.PutSecretMasking(c)
	}
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded, rec.Body.String()
}

func TestSecretMaskingSwitchIsPerTenantAndNeverShowsTheKey(t *testing.T) {
	newLoopGuardTestDB(t)
	registry := contentmoderation.NewSecretMaskingRegistry()
	registry.SetStore(settingsstore.SecretMaskingSettings{})
	h := &Handler{secretMasking: registry}

	if code, view, _ := secretMaskingCall(h, http.MethodGet, identity.SystemTenantID, ""); code != http.StatusOK || view["enabled"] != false || view["persistent"] != true {
		t.Fatalf("GET = %d %v", code, view)
	}
	code, view, raw := secretMaskingCall(h, http.MethodPut, identity.SystemTenantID, `{"enabled":true}`)
	if code != http.StatusOK || view["enabled"] != true || view["updated_at"] == nil {
		t.Fatalf("PUT = %d %s", code, raw)
	}
	payload, _, ok := usage.LoadRuntimeSettingForTenant(identity.SystemTenantID, contentmoderation.SecretMaskingSettingKey)
	var stored contentmoderation.SecretMaskingSettings
	if !ok || json.Unmarshal(payload, &stored) != nil || stored.Key == "" || !stored.Enabled {
		t.Fatalf("stored row = %s", payload)
	}
	if strings.Contains(raw, stored.Key) || strings.Contains(raw, `"key"`) {
		t.Fatalf("the response carries the key: %s", raw)
	}
	// Another tenant is unaffected.
	if _, view, _ := secretMaskingCall(h, http.MethodGet, loopGuardTenantB, ""); view["enabled"] != false {
		t.Fatalf("tenant B = %v", view)
	}
	if code, _, raw := secretMaskingCall(h, http.MethodPut, identity.SystemTenantID, `{}`); code != http.StatusBadRequest || !strings.Contains(raw, "invalid_request") {
		t.Fatalf("PUT {} = %d %s", code, raw)
	}
	// A fresh registry, as on another node, reads what was saved.
	fresh := contentmoderation.NewSecretMaskingRegistry()
	fresh.SetStore(settingsstore.SecretMaskingSettings{})
	if got := fresh.For(identity.SystemTenantID); !got.Enabled || got.Key != stored.Key {
		t.Fatalf("read back %+v", got)
	}
}

func TestSecretMaskingSwitchWithoutAStoreIsRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{secretMasking: contentmoderation.NewSecretMaskingRegistry()}
	if code, _, raw := secretMaskingCall(h, http.MethodPut, identity.SystemTenantID, `{"enabled":true}`); code != http.StatusServiceUnavailable || !strings.Contains(raw, "secret_masking_not_persistent") {
		t.Fatalf("PUT = %d %s", code, raw)
	}
}
