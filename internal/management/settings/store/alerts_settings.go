package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// AlertSettings keeps the three documents alerting persists for each tenant
// (settings, history, sent table) as runtime_settings rows of that tenant. It
// reads the runtime database at call time, so a pool swapped after it was
// attached is still the one used.
//
// Nothing new is stored in a table of its own: the rows share the settings
// table's versioning, which is what makes the version-checked writes work, and
// its cluster announcements (see alerting's domain registration).
type AlertSettings struct{}

// appendAttempts bounds the retries of an event append that keeps losing races.
const appendAttempts = 5

func (AlertSettings) Persistent() bool { return usage.ConfigStoreAvailable() }

func (AlertSettings) LoadSettings(tenantID string) (alerting.Settings, int64, bool, error) {
	payload, version, ok := usage.LoadRuntimeSettingForTenant(tenantID, alerting.SettingKey)
	if !ok {
		return alerting.Settings{}, 0, false, nil
	}
	var settings alerting.Settings
	if err := json.Unmarshal(payload, &settings); err != nil {
		// A row nobody can read must not make the page unusable: report it as
		// absent, at its real version, so that a write replaces it.
		log.Warnf("alerting: the stored settings of tenant %s are unreadable and will be replaced by the next save: %v", tenantID, err)
		return alerting.Settings{}, version, false, nil
	}
	return settings, version, true, nil
}

func (AlertSettings) SaveSettings(tenantID string, settings alerting.Settings, expected int64) error {
	return saveVersioned(tenantID, alerting.SettingKey, settings, expected)
}

func (AlertSettings) LoadSent(tenantID string) (alerting.SentState, int64, error) {
	payload, version, ok := usage.LoadRuntimeSettingForTenant(tenantID, alerting.SentKey)
	if !ok {
		return alerting.SentState{}, 0, nil
	}
	var state alerting.SentState
	if err := json.Unmarshal(payload, &state); err != nil {
		// The table only suppresses repeats; starting it again costs at most one
		// repeated message, and an engine that cannot read it would be stuck.
		log.Warnf("alerting: the sent table of tenant %s is unreadable and will be rebuilt: %v", tenantID, err)
		return alerting.SentState{}, version, nil
	}
	return state, version, nil
}

func (AlertSettings) SaveSent(tenantID string, state alerting.SentState, expected int64) error {
	return saveVersioned(tenantID, alerting.SentKey, state, expected)
}

func (AlertSettings) LoadEvents(tenantID string) ([]alerting.Event, error) {
	payload, _, ok := usage.LoadRuntimeSettingForTenant(tenantID, alerting.EventsKey)
	if !ok {
		return nil, nil
	}
	var events alerting.EventLog
	if err := json.Unmarshal(payload, &events); err != nil {
		return nil, fmt.Errorf("decode %s: %w", alerting.EventsKey, err)
	}
	return events.Items, nil
}

// AppendEvents adds events to the history under the same version check as every
// other write, retrying on a lost race: the loop and a management test message
// may append at the same moment, on different nodes.
func (AlertSettings) AppendEvents(tenantID string, events []alerting.Event) error {
	var lastErr error
	for attempt := 1; attempt <= appendAttempts; attempt++ {
		var current alerting.EventLog
		payload, version, ok := usage.LoadRuntimeSettingForTenant(tenantID, alerting.EventsKey)
		if ok {
			if err := json.Unmarshal(payload, &current); err != nil {
				// An unreadable history is replaced rather than left to block every
				// later entry.
				log.Warnf("alerting: the history of tenant %s is unreadable and will be replaced: %v", tenantID, err)
				current = alerting.EventLog{}
			}
		}
		lastErr = saveVersioned(tenantID, alerting.EventsKey, current.Merged(events), version)
		if lastErr == nil || !errors.Is(lastErr, alerting.ErrConflict) {
			return lastErr
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return lastErr
}

func (AlertSettings) SettingsOfAll() ([]alerting.TenantSettings, error) {
	payloads, err := usage.RuntimeSettingPayloadsByKey(alerting.SettingKey)
	if err != nil {
		return nil, err
	}
	out := make([]alerting.TenantSettings, 0, len(payloads))
	for tenantID, payload := range payloads {
		var settings alerting.Settings
		if err := json.Unmarshal(payload, &settings); err != nil {
			log.Debugf("alerting: skipping the unreadable settings of tenant %s: %v", tenantID, err)
			continue
		}
		out = append(out, alerting.TenantSettings{TenantID: tenantID, Settings: settings})
	}
	return out, nil
}

// saveVersioned writes one row against the version it was read at and turns the
// store's conflict into alerting's.
func saveVersioned(tenantID, key string, value any, expected int64) error {
	if expected == alerting.AnyVersion {
		expected = configsync.AnyVersion
	}
	if _, err := usage.CompareAndSwapRuntimeSetting(context.Background(), tenantID, key, value, expected); err != nil {
		if errors.Is(err, configsync.ErrVersionConflict) {
			return alerting.ErrConflict
		}
		return err
	}
	return nil
}
