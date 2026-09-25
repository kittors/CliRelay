package jobsnapshot

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process Store. Tests share one between services that
// play different nodes.
type MemoryStore struct {
	mu        sync.Mutex
	rows      map[string]*memoryRow
	now       func() time.Time
	lostAfter time.Duration
	saves     int
}

type memoryRow struct {
	snap      Snapshot
	expiresAt time.Time
	heartbeat time.Time
}

// NewMemoryStore returns an empty store on the wall clock.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]*memoryRow), now: time.Now, lostAfter: OwnerLostAfter}
}

// SetOwnerLostAfter changes how long an unfinished job may go without a
// heartbeat before it reads as lost.
func (m *MemoryStore) SetOwnerLostAfter(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.lostAfter = d
	}
}

// Saves counts successful writes, for asserting that progress is coalesced.
func (m *MemoryStore) Saves() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

func (m *MemoryStore) Save(_ context.Context, snap Snapshot, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if row := m.rows[snap.ID]; row != nil && (row.snap.Version >= snap.Version || row.snap.Kind != snap.Kind) {
		return nil
	}
	snap.Result = append([]byte(nil), snap.Result...)
	snap.Error = append([]byte(nil), snap.Error...)
	snap.OwnerLost = false
	m.rows[snap.ID] = &memoryRow{snap: snap, expiresAt: now.Add(ttl), heartbeat: now}
	m.saves++
	return nil
}

func (m *MemoryStore) Get(_ context.Context, kind, id string) (Snapshot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[id]
	now := m.now()
	if row == nil || row.snap.Kind != kind || !now.Before(row.expiresAt) {
		return Snapshot{}, false, nil
	}
	snap := row.snap
	snap.OwnerLost = !snap.Terminal && now.Sub(row.heartbeat) > m.lostAfter
	return snap, true, nil
}

func (m *MemoryStore) Touch(_ context.Context, owner string, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, id := range ids {
		if row := m.rows[id]; row != nil && row.snap.OwnerNode == owner && !row.snap.Terminal {
			row.heartbeat = now
		}
	}
	return nil
}
