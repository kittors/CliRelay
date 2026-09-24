package auth

import (
	"context"
	"errors"
	"strings"
	"time"
)

// credentialWriteAttempts bounds how often a write that lost a version race is
// re-applied onto the newest copy. Every retry means another writer committed
// in between, so running out takes sustained contention on one credential.
const credentialWriteAttempts = 4

func (m *Manager) versionedStore() VersionedStore {
	if m == nil {
		return nil
	}
	store, _ := m.store.(VersionedStore)
	return store
}

// pinStoreIndex applies the store-assigned auth_index to a persistable auth.
// The file store derives it from whatever FileName the registering code path
// set (OAuth saves use the basename, the store the relative path); a versioned
// store answers by ID so every node agrees on it. No-op for the file store.
func (m *Manager) pinStoreIndex(auth *Auth) {
	if m == nil || auth == nil || (auth.indexAssigned && auth.Index != "") {
		return
	}
	if auth.Metadata == nil || isRuntimeOnlyAuth(auth) {
		return
	}
	resolver, ok := m.store.(AuthIndexResolver)
	if !ok {
		return
	}
	if index := strings.TrimSpace(resolver.ResolveAuthIndex(auth.ID)); index != "" {
		auth.Index = index
		auth.indexAssigned = true
	}
}

func isRuntimeOnlyAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

// MutateAuth applies mutate to a credential and persists the result. base is
// the caller's copy, usually from GetByID. mutate reports whether it changed
// anything; nothing is saved when it did not.
//
// With the file store the mutation is applied to base and saved exactly as a
// plain Update. With a versioned store every attempt re-reads the newest
// persisted copy and applies mutate to that, so a change another node
// committed meanwhile is kept instead of being overwritten by base's content.
func (m *Manager) MutateAuth(ctx context.Context, base *Auth, mutate func(*Auth) (bool, error)) (*Auth, error) {
	if m == nil || base == nil || mutate == nil {
		return nil, nil
	}
	store := m.versionedStore()
	if store == nil {
		changed, err := mutate(base)
		if err != nil || !changed {
			return nil, err
		}
		return m.Update(ctx, base)
	}
	var lastErr error
	for attempt := 0; attempt < credentialWriteAttempts; attempt++ {
		target, err := m.latestForWrite(ctx, store, base.ID)
		if err != nil {
			return nil, err
		}
		changed, err := mutate(target)
		if err != nil || !changed {
			return nil, err
		}
		updated, err := m.Update(ctx, target)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, ErrCredentialConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// UpdateRefreshed persists a credential produced by a token refresh; before is
// the snapshot taken ahead of the refresh. With a versioned store, a write
// that lost a version race (or hit a transient store error) re-applies only
// the keys the refresh changed onto the newest copy: a rotated refresh token
// is often single-use, so dropping it would lock the account out, while an
// unrelated edit made on another node must survive. With the file store, or
// without a snapshot, it is a plain Update.
func (m *Manager) UpdateRefreshed(ctx context.Context, auth *Auth, before MetadataSnapshot) (*Auth, error) {
	if m == nil || auth == nil {
		return nil, nil
	}
	store := m.versionedStore()
	if store == nil || before == nil {
		return m.Update(ctx, auth)
	}
	set, removed := before.changes(auth.Metadata)
	updated, err := m.Update(ctx, auth)
	for attempt := 1; err != nil && attempt < credentialWriteAttempts; attempt++ {
		if errors.Is(err, ErrCredentialGone) {
			break
		}
		if !errors.Is(err, ErrCredentialConflict) && !sleepContext(ctx, time.Duration(attempt)*500*time.Millisecond) {
			break
		}
		target, errLatest := m.latestForWrite(ctx, store, auth.ID)
		if errLatest != nil {
			err = errLatest
			continue
		}
		applyMetadataChanges(target, set, removed)
		target.LastRefreshedAt = auth.LastRefreshedAt
		target.NextRefreshAfter = auth.NextRefreshAfter
		target.LastError = cloneError(auth.LastError)
		target.UpdatedAt = auth.UpdatedAt
		updated, err = m.Update(ctx, target)
	}
	return updated, err
}

// AdoptPersistedCredential installs latest, a newer persisted copy of a
// credential, as this node's copy without writing it back. The node keeps its
// scheduling state and runtime observations. It returns the installed copy.
func (m *Manager) AdoptPersistedCredential(ctx context.Context, latest *Auth) *Auth {
	if m == nil || latest == nil || latest.ID == "" {
		return latest
	}
	merged := latest.Clone()
	if current, ok := m.GetByID(latest.ID); ok && current != nil {
		merged = mergeLatestCredential(current, latest)
	}
	updated, err := m.Update(WithSkipPersist(ctx), merged)
	if err != nil || updated == nil {
		return merged
	}
	return updated
}

// latestForWrite returns the newest persisted copy of id merged with this
// node's runtime view of it, ready to be mutated and saved.
func (m *Manager) latestForWrite(ctx context.Context, store VersionedStore, id string) (*Auth, error) {
	latest, err := store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, ErrCredentialGone
	}
	current, ok := m.GetByID(id)
	if !ok || current == nil {
		restorePersistedQuotaRuntime(latest)
		return latest, nil
	}
	return mergeLatestCredential(current, latest), nil
}

// persistedAuthByID re-reads one credential from the store. A versioned store
// answers by ID; the file store has no lookup, so its whole listing is read
// exactly as before.
func (m *Manager) persistedAuthByID(ctx context.Context, id string) (*Auth, error) {
	if store := m.versionedStore(); store != nil {
		auth, err := store.Get(ctx, id)
		if err != nil || auth == nil {
			return nil, err
		}
		restorePersistedQuotaRuntime(auth)
		return auth, nil
	}
	items, err := m.loadPersistedAuths(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item != nil && item.ID == id {
			return item, nil
		}
	}
	return nil, nil
}

// mergeLatestCredential overlays the newest persisted credential on this
// node's copy. The document comes from latest, and so do the fields a save
// copies back into it (prefix, proxy, disabled): taking those from the older
// copy would silently revert another node's edit on the next save. Scheduling
// state, attributes and runtime observations stay the node's own.
func mergeLatestCredential(current, latest *Auth) *Auth {
	merged := current.Clone()
	merged.Metadata = make(map[string]any, len(latest.Metadata))
	for key, value := range latest.Metadata {
		merged.Metadata[key] = value
	}
	PreserveRuntimeMetadata(merged, current)
	merged.Prefix = latest.Prefix
	merged.ProxyURL = latest.ProxyURL
	merged.ProxyID = latest.ProxyID
	if label := metadataLabel(merged.Metadata); label != "" {
		merged.Label = label
	}
	switch {
	case latest.Disabled:
		merged.Disabled = true
		merged.Status = StatusDisabled
	case merged.Disabled || merged.Status == StatusDisabled:
		merged.Disabled = false
		merged.Status = StatusActive
		merged.StatusMessage = ""
	}
	if refreshedAt, ok := authLastRefreshTimestamp(latest); ok && refreshedAt.After(merged.LastRefreshedAt) {
		merged.LastRefreshedAt = refreshedAt
	}
	return merged
}

func metadataLabel(metadata map[string]any) string {
	for _, key := range []string{"label", "email"} {
		if value, ok := metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func applyMetadataChanges(target *Auth, set map[string]any, removed []string) {
	if target.Metadata == nil {
		target.Metadata = make(map[string]any, len(set))
	}
	for key, value := range set {
		target.Metadata[key] = value
	}
	for _, key := range removed {
		delete(target.Metadata, key)
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
