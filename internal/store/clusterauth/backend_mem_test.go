package clusterauth

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// memBackend mirrors the semantics of the PostgreSQL backend in memory, so
// the store logic can be exercised across simulated nodes without a server.
type memBackend struct {
	mu       sync.Mutex
	importMu sync.Mutex
	rows     map[string]*memRow
	bindings map[string]string
	// down makes every call fail, as an unreachable database would.
	down bool
}

type memRow struct {
	row
	refreshOwner string
	refreshUntil time.Time
}

var errMemDown = errors.New("mem backend: connection refused")

func newMemBackend() *memBackend {
	return &memBackend{rows: make(map[string]*memRow), bindings: make(map[string]string)}
}

func (b *memBackend) setDown(down bool) {
	b.mu.Lock()
	b.down = down
	b.mu.Unlock()
}

func (b *memBackend) close() error { return nil }

func (b *memBackend) withImportLock(ctx context.Context, fn func(context.Context) error) error {
	b.importMu.Lock()
	defer b.importMu.Unlock()
	return fn(ctx)
}

func (b *memBackend) count(context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return 0, errMemDown
	}
	return len(b.rows), nil
}

func cloneRow(r row) row {
	r.Content = append([]byte(nil), r.Content...)
	r.Runtime = append([]byte(nil), r.Runtime...)
	return r
}

func (b *memBackend) listLive(context.Context) ([]row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errMemDown
	}
	out := make([]row, 0, len(b.rows))
	for _, r := range b.rows {
		if !r.Deleted {
			out = append(out, cloneRow(r.row))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (b *memBackend) listVersions(context.Context) ([]rowVersion, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errMemDown
	}
	out := make([]rowVersion, 0, len(b.rows))
	for _, r := range b.rows {
		out = append(out, rowVersion{ID: r.ID, Version: r.Version, Deleted: r.Deleted})
	}
	return out, nil
}

func (b *memBackend) get(_ context.Context, id string) (*row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errMemDown
	}
	r, ok := b.rows[id]
	if !ok {
		return nil, nil
	}
	out := cloneRow(r.row)
	return &out, nil
}

func (b *memBackend) getMany(ctx context.Context, ids []string) ([]row, error) {
	out := make([]row, 0, len(ids))
	for _, id := range ids {
		r, err := b.get(ctx, id)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (b *memBackend) upsert(_ context.Context, r row, publish publishFunc) (row, error) {
	b.mu.Lock()
	if b.down {
		b.mu.Unlock()
		return row{}, errMemDown
	}
	now := time.Now()
	existing, ok := b.rows[r.ID]
	stored := cloneRow(r)
	stored.Deleted = false
	stored.UpdatedAt = now
	if ok {
		stored.Version = existing.Version + 1
		stored.CreatedAt = existing.CreatedAt
		if existing.AuthIndex != "" {
			stored.AuthIndex = existing.AuthIndex
		}
		existing.row = stored
	} else {
		stored.Version = 1
		stored.CreatedAt = now
		b.rows[r.ID] = &memRow{row: stored}
	}
	b.mu.Unlock()
	return stored, publish(nil, stored.Version)
}

func (b *memBackend) compareAndSwap(_ context.Context, id string, expected int64, r row, publish publishFunc) (int64, bool, error) {
	b.mu.Lock()
	if b.down {
		b.mu.Unlock()
		return 0, false, errMemDown
	}
	existing, ok := b.rows[id]
	if !ok || existing.Deleted || existing.Version != expected {
		b.mu.Unlock()
		return 0, false, nil
	}
	existing.Content = append([]byte(nil), r.Content...)
	existing.FileName, existing.Provider = r.FileName, r.Provider
	existing.Version++
	existing.UpdatedAt = time.Now()
	version := existing.Version
	b.mu.Unlock()
	return version, true, publish(nil, version)
}

func (b *memBackend) tombstone(_ context.Context, id, _ string, publish publishFunc) (int64, bool, error) {
	b.mu.Lock()
	if b.down {
		b.mu.Unlock()
		return 0, false, errMemDown
	}
	existing, ok := b.rows[id]
	if !ok || existing.Deleted {
		b.mu.Unlock()
		return 0, false, nil
	}
	existing.Deleted = true
	existing.Version++
	existing.Content, existing.Runtime = []byte(`{}`), []byte(`{}`)
	existing.refreshOwner, existing.refreshUntil = "", time.Time{}
	version := existing.Version
	b.mu.Unlock()
	return version, true, publish(nil, version)
}

func (b *memBackend) patchRuntime(_ context.Context, id, _ string, set map[string]json.RawMessage, removed []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return errMemDown
	}
	existing, ok := b.rows[id]
	if !ok || existing.Deleted {
		return nil
	}
	runtime := make(map[string]json.RawMessage)
	_ = json.Unmarshal(existing.Runtime, &runtime)
	for _, key := range removed {
		delete(runtime, key)
	}
	for key, raw := range set {
		runtime[key] = raw
	}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		return err
	}
	existing.Runtime = encoded
	return nil
}

func (b *memBackend) claimLease(_ context.Context, id, owner string, ttl time.Duration) (*row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errMemDown
	}
	existing, ok := b.rows[id]
	if !ok || existing.Deleted {
		return nil, coreauth.ErrCredentialGone
	}
	now := time.Now()
	if existing.refreshOwner != "" && existing.refreshOwner != owner && existing.refreshUntil.After(now) {
		return nil, nil
	}
	existing.refreshOwner, existing.refreshUntil = owner, now.Add(ttl)
	out := cloneRow(existing.row)
	return &out, nil
}

func (b *memBackend) releaseLease(_ context.Context, id, owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.rows[id]; ok && existing.refreshOwner == owner {
		existing.refreshOwner, existing.refreshUntil = "", time.Time{}
	}
	return nil
}

func (b *memBackend) insertImported(_ context.Context, rows []row) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return 0, errMemDown
	}
	inserted := 0
	for _, r := range rows {
		if _, ok := b.rows[r.ID]; ok {
			continue
		}
		stored := cloneRow(r)
		stored.Version = 1
		stored.CreatedAt = r.UpdatedAt
		b.rows[r.ID] = &memRow{row: stored}
		inserted++
	}
	return inserted, nil
}

func (b *memBackend) activeBindingIndexes(context.Context) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.bindings))
	for key, value := range b.bindings {
		out[key] = value
	}
	return out, nil
}

// rollback simulates a failover that lost the last write of id: the row goes
// back to version and content.
func (b *memBackend) rollback(id string, version int64, content []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.rows[id]; ok {
		existing.Version = version
		existing.Content = append([]byte(nil), content...)
		existing.Deleted = false
	}
}
