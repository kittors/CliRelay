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

// AttachSecretMaskingSettings attaches the store to the registry of this
// process when a database is available. It is safe to call from every place
// that assembles the gateway, as each of them may be the only one running.
func AttachSecretMaskingSettings() {
	if usage.RuntimeDB() == nil || contentmoderation.SecretMasking().Persistent() {
		return
	}
	contentmoderation.SecretMasking().SetStore(SecretMaskingSettings{})
}
