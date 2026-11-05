package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// SecretMaskingSettings stores a tenant's outbound secret masking settings as a
// runtime_settings row of that tenant. It reads the runtime database at call
// time, so a pool swapped after it was attached is still the one used.
type SecretMaskingSettings struct{}

func (SecretMaskingSettings) Load(ctx context.Context, tenantID string) (contentmoderation.SecretMaskingSettings, int64, error) {
	// A failed read must not pass for "no row": that is masking off, and the
	// tenant's secrets would go upstream until the next read.
	payload, version, ok, err := usage.LoadRuntimeSettingForTenantContext(ctx, tenantID, contentmoderation.SecretMaskingSettingKey)
	if err != nil {
		return contentmoderation.SecretMaskingSettings{}, 0, fmt.Errorf("load %s: %w", contentmoderation.SecretMaskingSettingKey, err)
	}
	if !ok || len(payload) == 0 {
		return contentmoderation.SecretMaskingSettings{}, 0, nil
	}
	var settings contentmoderation.SecretMaskingSettings
	if err := json.Unmarshal(payload, &settings); err != nil {
		return contentmoderation.SecretMaskingSettings{}, 0, fmt.Errorf("decode %s: %w", contentmoderation.SecretMaskingSettingKey, err)
	}
	return settings, version, nil
}

// Save writes the row only if it is still at the version expected, and announces
// the write to the other nodes in the same transaction.
func (SecretMaskingSettings) Save(ctx context.Context, tenantID string, settings contentmoderation.SecretMaskingSettings, expected int64) error {
	if !usage.ConfigStoreAvailable() {
		return contentmoderation.ErrSecretMaskingNotPersistent
	}
	if _, err := usage.CompareAndSwapRuntimeSetting(ctx, tenantID, contentmoderation.SecretMaskingSettingKey, settings, expected); err != nil {
		if errors.Is(err, configsync.ErrVersionConflict) {
			return contentmoderation.ErrSecretMaskingConflict
		}
		return err
	}
	return nil
}

// LoadAll returns every tenant's stored settings, keyed by tenant id: the
// registry reads them once at start (Preload), so a tenant whose row cannot be
// read on its first request is still known to have masking on or off.
func (SecretMaskingSettings) LoadAll(context.Context) (map[string]contentmoderation.SecretMaskingSettings, error) {
	payloads, err := usage.RuntimeSettingPayloadsByKey(contentmoderation.SecretMaskingSettingKey)
	if err != nil {
		return nil, fmt.Errorf("load every %s: %w", contentmoderation.SecretMaskingSettingKey, err)
	}
	out := make(map[string]contentmoderation.SecretMaskingSettings, len(payloads))
	for tenantID, payload := range payloads {
		if len(payload) == 0 {
			continue
		}
		var settings contentmoderation.SecretMaskingSettings
		if err := json.Unmarshal(payload, &settings); err != nil {
			// One bad row must not stop the others from being known; the tenant's
			// own read reports it.
			continue
		}
		out[tenantID] = settings
	}
	return out, nil
}

// AttachSecretMaskingSettings attaches the store to the registry of this
// process when a database is available, and reads every tenant's settings in
// the background. It is safe to call from every place that assembles the
// gateway, as each of them may be the only one running.
func AttachSecretMaskingSettings() {
	if usage.RuntimeDB() == nil || contentmoderation.SecretMasking().Persistent() {
		return
	}
	contentmoderation.SecretMasking().SetStore(SecretMaskingSettings{})
	contentmoderation.SecretMasking().PreloadInBackground()
}
