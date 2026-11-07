package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// LoopGuardSettings stores a tenant's loop protection settings as a
// runtime_settings row of that tenant. It reads the runtime database at call
// time, so a pool swapped after it was attached is still the one used.
type LoopGuardSettings struct{}

func (LoopGuardSettings) Load(_ context.Context, tenantID string) (loopguard.Stored, error) {
	payload, version, ok := usage.LoadRuntimeSettingForTenant(tenantID, loopguard.SettingKey)
	if !ok || len(payload) == 0 {
		return loopguard.Stored{}, nil
	}
	var settings loopguard.Settings
	if err := json.Unmarshal(payload, &settings); err != nil {
		return loopguard.Stored{}, fmt.Errorf("decode %s: %w", loopguard.SettingKey, err)
	}
	return loopguard.Stored{Settings: settings, Version: version}, nil
}

// Save writes the row only if it is still at the version expected, and announces
// the write to the other nodes in the same transaction.
func (LoopGuardSettings) Save(ctx context.Context, tenantID string, settings loopguard.Settings, expected int64) error {
	if !usage.ConfigStoreAvailable() {
		return loopguard.ErrNotPersistent
	}
	if _, err := usage.CompareAndSwapRuntimeSetting(ctx, tenantID, loopguard.SettingKey, settings, expected); err != nil {
		if errors.Is(err, configsync.ErrVersionConflict) {
			return loopguard.ErrConflict
		}
		return err
	}
	return nil
}

// AttachLoopGuardSettings attaches the store to the registry of this process
// when a database is available. It is safe to call from every place that
// assembles the gateway, as each of them may be the only one running.
func AttachLoopGuardSettings() {
	if usage.RuntimeDB() == nil || loopguard.Default().Persistent() {
		return
	}
	loopguard.Default().SetStore(LoopGuardSettings{})
}
