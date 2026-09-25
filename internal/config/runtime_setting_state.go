package config

import (
	"bytes"
	"sync"
)

// RuntimeSettingSnapshot is what a config holds for one runtime_settings key:
// the canonical JSON of the value it was loaded with and the row version.
type RuntimeSettingSnapshot struct {
	Canonical []byte
	Version   int64
}

// RuntimeSettingState remembers which runtime_settings rows a config was
// built from. A management save compares the live config against it to find
// the keys the request actually changed, and uses the recorded version for the
// compare-and-swap, so a key another node rewrote meanwhile becomes a conflict
// instead of being silently reverted to this node's stale copy.
type RuntimeSettingState struct {
	mu      sync.Mutex
	entries map[string]RuntimeSettingSnapshot
}

// Get returns the snapshot recorded for key.
func (s *RuntimeSettingState) Get(key string) (RuntimeSettingSnapshot, bool) {
	if s == nil {
		return RuntimeSettingSnapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.entries[key]
	if !ok {
		return RuntimeSettingSnapshot{}, false
	}
	snap.Canonical = bytes.Clone(snap.Canonical)
	return snap, true
}

// Set records the snapshot for key.
func (s *RuntimeSettingState) Set(key string, snap RuntimeSettingSnapshot) {
	if s == nil {
		return
	}
	snap.Canonical = bytes.Clone(snap.Canonical)
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[string]RuntimeSettingSnapshot)
	}
	s.entries[key] = snap
	s.mu.Unlock()
}

// Forget drops the snapshot for key, used when the key has no stored row.
func (s *RuntimeSettingState) Forget(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// Version returns the recorded version for key, 0 when none is recorded.
func (s *RuntimeSettingState) Version(key string) int64 {
	snap, _ := s.Get(key)
	return snap.Version
}

// Versions returns every recorded version, for management read responses.
func (s *RuntimeSettingState) Versions() map[string]int64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.entries))
	for key, snap := range s.entries {
		out[key] = snap.Version
	}
	return out
}

// runtimeSettingStateMu guards the lazy creation below. A package-wide lock is
// enough: creation happens once per config object.
var runtimeSettingStateMu sync.Mutex

// RuntimeSettingState returns the state attached to cfg, creating it on first
// use. Value copies of a Config share the state; a copy that is going to be
// loaded independently (a tenant view) must call DetachRuntimeSettingState.
func (cfg *Config) RuntimeSettingState() *RuntimeSettingState {
	if cfg == nil {
		return nil
	}
	runtimeSettingStateMu.Lock()
	defer runtimeSettingStateMu.Unlock()
	if cfg.runtimeSettings == nil {
		cfg.runtimeSettings = &RuntimeSettingState{}
	}
	return cfg.runtimeSettings
}

// DetachRuntimeSettingState gives cfg a fresh, empty state so that loading it
// cannot disturb the state of the config it was copied from.
func (cfg *Config) DetachRuntimeSettingState() {
	if cfg == nil {
		return
	}
	runtimeSettingStateMu.Lock()
	cfg.runtimeSettings = &RuntimeSettingState{}
	runtimeSettingStateMu.Unlock()
}
