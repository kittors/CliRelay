package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	log "github.com/sirupsen/logrus"
)

// PendingChange is a runtime setting whose value in a config differs from the
// value the config was loaded with.
type PendingChange struct {
	Spec      runtimeconfig.Spec
	Canonical []byte
	// Expected is the version the config was loaded with, 0 when no row was.
	Expected int64
}

// DiffAgainstState lists the settings cfg changed relative to what state
// recorded when cfg was loaded. Keys that are merely stale (changed in the
// database by someone else, untouched here) compare equal and are left alone,
// which is what keeps a save from reverting another node's change.
func DiffAgainstState(cfg *config.Config, state *config.RuntimeSettingState) []PendingChange {
	if cfg == nil {
		return nil
	}
	var out []PendingChange
	for _, spec := range runtimeconfig.Specs() {
		current, err := runtimeconfig.Canonical(spec, cfg)
		if err != nil {
			log.Warnf("sqlite/settings: encode runtime setting %s: %v", spec.Key, err)
			continue
		}
		snap, ok := state.Get(spec.Key)
		if ok && bytes.Equal(current, snap.Canonical) {
			continue
		}
		if !ok && !spec.Meaningful(cfg) {
			// Nothing stored and nothing configured: no row is needed.
			continue
		}
		out = append(out, PendingChange{Spec: spec, Canonical: current, Expected: snap.Version})
	}
	return out
}

// CommitChanges writes every setting cfg changed relative to its recorded
// state, in one transaction, each checked against the version it was loaded
// with. When the client named the version it edited (clientVersion != nil) and
// exactly one key changed, that version is checked instead. On success the
// state is updated to the new versions; on a conflict nothing is written. The
// returned keys are the ones attempted, also on failure, so a caller can
// reload them.
func CommitChanges(ctx context.Context, store RuntimeSettingsStore, cfg *config.Config, clientVersion *int64) ([]string, error) {
	state := cfg.RuntimeSettingState()
	pending := DiffAgainstState(cfg, state)
	if len(pending) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(pending))
	changes := make([]Change, 0, len(pending))
	for _, p := range pending {
		keys = append(keys, p.Spec.Key)
		changes = append(changes, Change{Key: p.Spec.Key, Value: p.Spec.Value(cfg), Expected: p.Expected})
	}
	if clientVersion != nil && len(changes) == 1 {
		changes[0].Expected = *clientVersion
	}
	versions, err := store.CompareAndSwap(ctx, changes)
	if err != nil {
		return keys, err
	}
	for _, p := range pending {
		state.Set(p.Spec.Key, config.RuntimeSettingSnapshot{Canonical: p.Canonical, Version: versions[p.Spec.Key]})
	}
	return keys, nil
}

// ReloadKeys re-reads keys from the database into cfg and its state. After a
// rejected save it puts the live config back on the stored values, so the
// node neither serves nor later re-submits the value that lost.
func ReloadKeys(store RuntimeSettingsStore, cfg *config.Config, keys ...string) {
	state := cfg.RuntimeSettingState()
	for _, key := range keys {
		spec, ok := runtimeconfig.SpecByKey(key)
		if !ok {
			continue
		}
		entry, ok := store.Load(key)
		if !ok {
			state.Forget(key)
			continue
		}
		if spec.Apply(cfg, entry.Payload) {
			RecordSnapshot(state, spec, cfg, entry.Version)
		}
	}
}

// providerIDKeys are the settings whose entries carry stable provider IDs.
var providerIDKeys = []string{
	runtimeconfig.RuntimeSettingGeminiKeys,
	runtimeconfig.RuntimeSettingCodexKeys,
	runtimeconfig.RuntimeSettingClaudeKeys,
	runtimeconfig.RuntimeSettingBedrockKeys,
	runtimeconfig.RuntimeSettingOpenCodeGoKeys,
	runtimeconfig.RuntimeSettingClineKeys,
	runtimeconfig.RuntimeSettingOllamaCloudKeys,
	runtimeconfig.RuntimeSettingCommandCodeKeys,
	runtimeconfig.RuntimeSettingOpenAICompatibility,
	runtimeconfig.RuntimeSettingVertexCompatKeys,
}

// EnsureStoredProviderIDs gives provider entries loaded without IDs their
// deterministic IDs and writes them back, each key against the version it was
// loaded with. This is a derived write, not a client edit, so a conflict is
// resolved by adopting the newer stored value, deriving again and retrying.
// cfg must have been loaded with state recording (ApplyToConfigRecording).
func EnsureStoredProviderIDs(ctx context.Context, store RuntimeSettingsStore, cfg *config.Config) bool {
	if cfg == nil || !cfg.EnsureProviderStableIDsDeterministic(store.TenantID()) {
		return false
	}
	state := cfg.RuntimeSettingState()
	for _, key := range providerIDKeys {
		spec, ok := runtimeconfig.SpecByKey(key)
		if !ok {
			continue
		}
		snap, stored := state.Get(key)
		if !stored {
			// No row: the IDs belong to a config.yaml value that migration
			// will import with them.
			continue
		}
		backfillProviderIDs(ctx, store, cfg, state, spec, snap)
	}
	return true
}

func backfillProviderIDs(ctx context.Context, store RuntimeSettingsStore, cfg *config.Config, state *config.RuntimeSettingState, spec runtimeconfig.Spec, snap config.RuntimeSettingSnapshot) {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := runtimeconfig.Canonical(spec, cfg)
		if err != nil {
			return
		}
		if bytes.Equal(current, snap.Canonical) {
			state.Set(spec.Key, config.RuntimeSettingSnapshot{Canonical: current, Version: snap.Version})
			return
		}
		versions, err := store.CompareAndSwap(ctx, []Change{{Key: spec.Key, Value: spec.Value(cfg), Expected: snap.Version}})
		if err == nil {
			state.Set(spec.Key, config.RuntimeSettingSnapshot{Canonical: current, Version: versions[spec.Key]})
			return
		}
		if !errors.Is(err, configsync.ErrVersionConflict) {
			log.WithError(err).Warnf("sqlite/settings: persist provider IDs for %s", spec.Key)
			// Keep the derived IDs in memory without re-submitting them on the
			// next save; the next load derives the same IDs and tries again.
			state.Set(spec.Key, config.RuntimeSettingSnapshot{Canonical: current, Version: snap.Version})
			return
		}
		entry, ok := store.Load(spec.Key)
		if !ok || !spec.Apply(cfg, entry.Payload) {
			return
		}
		loaded, err := runtimeconfig.Canonical(spec, cfg)
		if err != nil {
			return
		}
		cfg.EnsureProviderStableIDsDeterministic(store.TenantID())
		snap = config.RuntimeSettingSnapshot{Canonical: loaded, Version: entry.Version}
	}
	current, _ := runtimeconfig.Canonical(spec, cfg)
	state.Set(spec.Key, config.RuntimeSettingSnapshot{Canonical: current, Version: snap.Version})
}

// Rebaseline records the current value of every setting in cfg as its
// baseline, keeping the loaded versions (0 where no row exists). It runs after
// a config has been fully assembled, so normalisation applied after loading
// (identity fingerprint defaults, derived provider IDs) is not mistaken for an
// edit by the next save.
func Rebaseline(cfg *config.Config) {
	if cfg == nil {
		return
	}
	state := cfg.RuntimeSettingState()
	for _, spec := range runtimeconfig.Specs() {
		RecordSnapshot(state, spec, cfg, state.Version(spec.Key))
	}
}

// AdoptKeys copies keys from src, a config whose write just committed, into
// dst, the live config, together with the versions src recorded. The live
// config then matches the database without waiting for a reload.
func AdoptKeys(dst, src *config.Config, keys ...string) {
	if dst == nil || src == nil {
		return
	}
	srcState, dstState := src.RuntimeSettingState(), dst.RuntimeSettingState()
	for _, key := range keys {
		spec, ok := runtimeconfig.SpecByKey(key)
		if !ok {
			continue
		}
		raw, err := json.Marshal(spec.Value(src))
		if err != nil {
			continue
		}
		if spec.Apply(dst, raw) {
			RecordSnapshot(dstState, spec, dst, srcState.Version(key))
		}
	}
}
