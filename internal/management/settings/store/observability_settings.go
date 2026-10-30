package store

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// ObservabilitySettings stores the metrics endpoint settings as a
// runtime_settings row of the system tenant. It reads the runtime database at
// call time, so a pool swapped after it was attached is still the one used.
type ObservabilitySettings struct{}

func (ObservabilitySettings) Load() (observability.Settings, bool, error) {
	payload, ok := usage.GetRuntimeSettingPayload(observability.SettingKey)
	if !ok || len(payload) == 0 {
		return observability.Settings{}, false, nil
	}
	var settings observability.Settings
	if err := json.Unmarshal(payload, &settings); err != nil {
		return observability.Settings{}, false, fmt.Errorf("decode %s: %w", observability.SettingKey, err)
	}
	return settings, true, nil
}

func (ObservabilitySettings) Save(settings observability.Settings) error {
	return usage.UpsertRuntimeSetting(observability.SettingKey, settings)
}
