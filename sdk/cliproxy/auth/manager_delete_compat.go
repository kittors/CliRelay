package auth

import (
	"context"
	"strings"
)

// Delete removes an auth from runtime state and, unless persistence is disabled
// on the context, from the backing store as well.
func (m *Manager) Delete(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	auth, ok := m.GetByID(id)
	if !ok || auth == nil {
		return nil, nil
	}
	m.Remove(ctx, id)
	if !shouldSkipPersist(ctx) {
		m.mu.RLock()
		store := m.store
		m.mu.RUnlock()
		if store != nil {
			if err := store.Delete(ctx, id); err != nil {
				return auth, err
			}
		}
	}
	return auth, nil
}
