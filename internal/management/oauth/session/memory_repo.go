package session

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryRepo is an in-process Repo. Tests use it to stand in for the shared
// database when several ClusterStores play the nodes of one cluster.
type MemoryRepo struct {
	mu        sync.Mutex
	rows      map[string]*memoryRow
	now       func() time.Time
	lostAfter time.Duration
}

type memoryRow struct {
	rec       Record
	callback  map[string]string
	heartbeat time.Time
}

// NewMemoryRepo returns an empty repository using the wall clock.
func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{rows: make(map[string]*memoryRow), now: time.Now, lostAfter: OwnerLostAfter}
}

// SetClock replaces the repository clock.
func (r *MemoryRepo) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now != nil {
		r.now = now
	}
}

// SetOwnerLostAfter changes how long a pending session may go without a
// heartbeat before its owner counts as lost.
func (r *MemoryRepo) SetOwnerLostAfter(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d > 0 {
		r.lostAfter = d
	}
}

// States lists the stored states, for assertions.
func (r *MemoryRepo) States() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rows))
	for state := range r.rows {
		out = append(out, state)
	}
	sort.Strings(out)
	return out
}

func (r *MemoryRepo) Create(_ context.Context, rec Record, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	rec.Status = RecordPending
	rec.Error = ""
	rec.CreatedAt = now
	rec.ExpiresAt = now.Add(ttl)
	r.rows[rec.State] = &memoryRow{rec: rec, heartbeat: now}
	return nil
}

func (r *MemoryRepo) Get(_ context.Context, state string) (Record, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[state]
	if row == nil {
		return Record{}, false, nil
	}
	return r.viewLocked(row), true, nil
}

func (r *MemoryRepo) viewLocked(row *memoryRow) Record {
	now := r.now()
	rec := row.rec
	rec.Expired = !now.Before(rec.ExpiresAt)
	rec.OwnerLost = rec.Status == RecordPending && now.Sub(row.heartbeat) > r.lostAfter
	return rec
}

func (r *MemoryRepo) livePendingLocked(state string) *memoryRow {
	row := r.rows[state]
	if row == nil {
		return nil
	}
	view := r.viewLocked(row)
	if view.Status != RecordPending || view.Expired {
		return nil
	}
	return row
}

func (r *MemoryRepo) Deliver(_ context.Context, state, provider string, payload map[string]string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.livePendingLocked(state)
	if row == nil || !strings.EqualFold(row.rec.Provider, provider) || r.viewLocked(row).OwnerLost {
		return false, nil
	}
	row.callback = copyPayload(payload)
	if grace := r.now().Add(CallbackGrace); grace.After(row.rec.ExpiresAt) {
		row.rec.ExpiresAt = grace
	}
	return true, nil
}

func (r *MemoryRepo) TakeCallback(_ context.Context, state string) (map[string]string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[state]
	if row == nil || row.rec.Status != RecordPending || row.callback == nil {
		return nil, false, nil
	}
	payload := row.callback
	row.callback = nil
	return payload, true, nil
}

func (r *MemoryRepo) Finish(_ context.Context, state, status, message string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[state]
	if row == nil || row.rec.Status != RecordPending {
		return false, nil
	}
	r.finishLocked(row, status, message, ttl)
	return true, nil
}

func (r *MemoryRepo) finishLocked(row *memoryRow, status, message string, ttl time.Duration) {
	row.rec.Status = status
	row.rec.Error = message
	row.rec.ExpiresAt = r.now().Add(ttl)
	row.callback = nil
}

func (r *MemoryRepo) CancelPending(_ context.Context, provider, tenantID, message string, ttl time.Duration) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var states []string
	for state := range r.rows {
		row := r.livePendingLocked(state)
		if row == nil || !strings.EqualFold(row.rec.Provider, provider) {
			continue
		}
		if tenantID != "" && row.rec.TenantID != tenantID {
			continue
		}
		r.finishLocked(row, RecordCancelled, message, ttl)
		states = append(states, state)
	}
	sort.Strings(states)
	return states, nil
}

func (r *MemoryRepo) Heartbeat(_ context.Context, state, owner string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.livePendingLocked(state)
	if row == nil || row.rec.OwnerNode != owner {
		return false, nil
	}
	row.heartbeat = r.now()
	return true, nil
}

func copyPayload(payload map[string]string) map[string]string {
	out := make(map[string]string, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	return out
}
