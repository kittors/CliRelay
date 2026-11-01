package alerting

import (
	"errors"
	"sync"
	"time"
)

// AnyVersion makes a save apply to whatever is stored, like configsync.AnyVersion
// (which it maps to in the database store).
const AnyVersion int64 = -1

var (
	// ErrConflict is returned by a versioned save that lost a race with another
	// writer.
	ErrConflict = errors.New("alerting: the stored state changed underneath this write")
	// ErrNotPersistent is returned by writes when no settings store is attached: a
	// setting that took effect but would be gone after the next restart is worse
	// than one that was refused.
	ErrNotPersistent = errors.New("alerting settings cannot be saved: no settings store is attached")
)

// TenantSettings is one tenant's stored settings.
type TenantSettings struct {
	TenantID string
	Settings Settings
}

// Store keeps the three documents alerting persists per tenant (see SettingKey,
// EventsKey and SentKey). It is an interface so this package never depends on
// the settings database.
//
// Settings and the sent table are saved against the version they were loaded
// at, because both are read-modify-write and have more than one writer (two
// administrators; two backend slots during a deploy). The history is only ever
// appended to, so AppendEvents merges under the store's own retry.
type Store interface {
	// Persistent reports whether writes survive a restart.
	Persistent() bool

	// LoadSettings returns the stored settings, the version they were read at
	// (0 when none were ever saved) and whether a row exists.
	LoadSettings(tenantID string) (Settings, int64, bool, error)
	// SaveSettings replaces the settings if they are still at expected (0: absent,
	// AnyVersion: unchecked) and returns ErrConflict otherwise.
	SaveSettings(tenantID string, settings Settings, expected int64) error

	LoadSent(tenantID string) (SentState, int64, error)
	SaveSent(tenantID string, state SentState, expected int64) error

	// AppendEvents adds events to the front of the history.
	AppendEvents(tenantID string, events []Event) error
	// LoadEvents returns the history, newest first.
	LoadEvents(tenantID string) ([]Event, error)

	// SettingsOfAll returns the settings of every tenant that has any, for the
	// evaluation loop to pick the tenants that have alerting on.
	SettingsOfAll() ([]TenantSettings, error)
}

// MemoryStore is a Store that keeps everything in memory. It backs the tests and
// is what a caller that has no database passes when it wants the service to
// work but not to persist.
type MemoryStore struct {
	mu       sync.Mutex
	settings map[string]versioned[Settings]
	sent     map[string]versioned[SentState]
	events   map[string]EventLog
	// Persist is what Persistent reports.
	Persist bool
	// FailWrites makes every write fail, for tests of the error paths.
	FailWrites error
}

type versioned[T any] struct {
	value   T
	version int64
}

// NewMemoryStore returns an empty store that reports itself persistent, so the
// service accepts writes to it.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		settings: map[string]versioned[Settings]{},
		sent:     map[string]versioned[SentState]{},
		events:   map[string]EventLog{},
		Persist:  true,
	}
}

func (m *MemoryStore) Persistent() bool { return m.Persist }

func (m *MemoryStore) LoadSettings(tenantID string) (Settings, int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.settings[tenantID]
	return cloneSettings(entry.value), entry.version, ok, nil
}

func (m *MemoryStore) SaveSettings(tenantID string, settings Settings, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	entry := m.settings[tenantID]
	if expected != AnyVersion && expected != entry.version {
		return ErrConflict
	}
	m.settings[tenantID] = versioned[Settings]{value: cloneSettings(settings), version: entry.version + 1}
	return nil
}

func (m *MemoryStore) LoadSent(tenantID string) (SentState, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.sent[tenantID]
	return cloneSent(entry.value), entry.version, nil
}

func (m *MemoryStore) SaveSent(tenantID string, state SentState, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	entry := m.sent[tenantID]
	if expected != AnyVersion && expected != entry.version {
		return ErrConflict
	}
	m.sent[tenantID] = versioned[SentState]{value: cloneSent(state), version: entry.version + 1}
	return nil
}

func (m *MemoryStore) AppendEvents(tenantID string, events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	m.events[tenantID] = m.events[tenantID].Merged(events)
	return nil
}

func (m *MemoryStore) LoadEvents(tenantID string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events[tenantID].Items...), nil
}

func (m *MemoryStore) SettingsOfAll() ([]TenantSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantSettings, 0, len(m.settings))
	for tenantID, entry := range m.settings {
		out = append(out, TenantSettings{TenantID: tenantID, Settings: cloneSettings(entry.value)})
	}
	return out, nil
}

func cloneSettings(s Settings) Settings {
	s.Channels = append([]Channel(nil), s.Channels...)
	return s
}

func cloneSent(s SentState) SentState {
	out := SentState{Records: make(map[string]SentRecord, len(s.Records))}
	for key, record := range s.Records {
		out.Records[key] = record
	}
	return out
}

// retrySleep is how long the versioned writers wait between attempts.
var retrySleep = func(attempt int) { time.Sleep(time.Duration(attempt) * 20 * time.Millisecond) }
