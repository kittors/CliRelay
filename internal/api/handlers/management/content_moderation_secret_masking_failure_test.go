package management

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

type secretMaskingUnavailableStore struct {
	settings contentmoderation.SecretMaskingSettings
	fail     bool
}

func (s *secretMaskingUnavailableStore) Load(context.Context, string) (contentmoderation.SecretMaskingSettings, int64, error) {
	if s.fail {
		return contentmoderation.SecretMaskingSettings{}, 0, errors.New("database unavailable")
	}
	return s.settings, 0, nil
}
func (s *secretMaskingUnavailableStore) Save(_ context.Context, _ string, v contentmoderation.SecretMaskingSettings, _ int64) error {
	s.settings = v
	return nil
}

func TestSecretMaskGetRefusesUnknownColdPolicy(t *testing.T) {
	store := &secretMaskingUnavailableStore{}
	writer := contentmoderation.NewSecretMaskingRegistry()
	writer.SetStore(store)
	if _, err := writer.SetEnabled(context.Background(), "tenant-a", true); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	fresh := contentmoderation.NewSecretMaskingRegistry()
	fresh.SetStore(store)
	h := &Handler{secretMasking: fresh}
	code, view, _ := secretMaskingCall(h, http.MethodGet, "tenant-a", "")
	t.Logf("persisted enabled=%v response=%d view=%v", store.settings.Enabled, code, view)
	if code != http.StatusServiceUnavailable || view["enabled"] != nil || view["error"] == nil {
		t.Fatal("unknown settings must not be reported as disabled")
	}
}
