package loopguard

import (
	"context"
	"sync"
)

// MemoryStore is a Store held in memory, with the versioning of the database
// one. It is for tests of the code around the registry, which need settings to
// read and write without a database.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Stored
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Stored)}
}

// Load implements Store.
func (m *MemoryStore) Load(_ context.Context, tenantID string) (Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.rows[normalizeTenant(tenantID)]
	stored.Settings = stored.Settings.Clone()
	return stored, nil
}

// Save implements Store.
func (m *MemoryStore) Save(_ context.Context, tenantID string, settings Settings, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenantID = normalizeTenant(tenantID)
	if m.rows[tenantID].Version != expected {
		return ErrConflict
	}
	m.rows[tenantID] = Stored{Settings: settings.Clone(), Version: expected + 1}
	return nil
}
