package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

// switchableMaskSettings fails its reads while down is set.
type switchableMaskSettings struct {
	memoryMaskSettings
	down *atomic.Bool
}

func (s switchableMaskSettings) Load(ctx context.Context, tenantID string) (contentmoderation.SecretMaskingSettings, int64, error) {
	if s.down.Load() {
		return contentmoderation.SecretMaskingSettings{}, 0, errors.New("database is down")
	}
	return s.memoryMaskSettings.Load(ctx, tenantID)
}

// While a tenant's settings are not known (never read, and the store does not
// answer) no view shows them as "nothing masked": the tenant's requests are
// refused meanwhile, and a view saying masking is off would be wrong twice.
func TestPrivacyMaskingViewsRefuseSettingsThatAreNotKnown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	down := &atomic.Bool{}
	store := switchableMaskSettings{memoryMaskSettings{rows: map[string]contentmoderation.SecretMaskingSettings{}, versions: map[string]int64{}}, down}
	writer := contentmoderation.NewSecretMaskingRegistry()
	writer.SetStore(store)
	if _, err := writer.SetEnabled(context.Background(), wordTenantA, true); err != nil {
		t.Fatal(err)
	}
	registry := contentmoderation.NewSecretMaskingRegistry() // a restart: nothing read yet
	registry.SetStore(store)
	down.Store(true)
	h := &Handler{secretMasking: registry}

	for _, tc := range []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		body    string
		code    string
	}{
		{"the old switch", h.GetSecretMasking, http.MethodGet, "", "secret_masking_unavailable"},
		{"the settings", h.GetPrivacyMaskingSettings, http.MethodGet, "", "privacy_masking_unavailable"},
		{"a preview of the saved policy", h.PostPrivacyMaskingPreview, http.MethodPost, `{"text":"hello"}`, "privacy_masking_unavailable"},
	} {
		rec, body := wordTenantAUser.call(tc.handler, tc.method, tc.body, "")
		if rec.Code != http.StatusServiceUnavailable || wordErrorCode(body) != tc.code || strings.Contains(rec.Body.String(), `"enabled"`) || strings.Contains(rec.Body.String(), `"policy"`) {
			t.Fatalf("%s while the settings are not known: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if _, known := h.portalMaskSettingsView(wordTenantA); known {
		t.Fatal("the portal's view of settings that are not known")
	}
	// A draft policy needs nothing stored.
	rec, _ := wordTenantAUser.call(h.PostPrivacyMaskingPreview, http.MethodPost, `{"text":"hello","policy":{"secrets":true}}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	// Once read, they are shown, and kept if the store fails afterwards.
	down.Store(false)
	registry.Invalidate(wordTenantA)
	for range 2 {
		rec, body := wordTenantAUser.call(h.GetSecretMasking, http.MethodGet, "", "")
		wordExpectStatus(t, rec, http.StatusOK)
		if body["enabled"] != true {
			t.Fatalf("the old switch once read: %v", body)
		}
		if view, known := h.portalMaskSettingsView(wordTenantA); !known || !view.Policy.Secrets {
			t.Fatalf("the portal's view once read: %+v, known %v", view, known)
		}
		down.Store(true)
		registry.Invalidate(wordTenantA)
	}
}
