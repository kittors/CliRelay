package contentmoderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// A portal user may keep a privacy masking policy of their own, for every
// request of their API keys, unless the tenant forbids it: the same two-step
// decision as for sensitive words (wordguard_portal.go), a default on the
// tenant's settings (SecretMaskingSettings.PortalDefaultAllowed) and an
// override per user here. A user the tenant stops allowing keeps the policy,
// which stops applying at once: the tenant's default applies instead.
//
// Like the tenant's settings, a user's are needed on every request of theirs,
// and a policy that cannot be read may be one that masks: MaskUsers reads them
// all at start and keeps them cached, and a user never read whose row cannot
// be read now has the request refused, not sent unmasked.

const privacyMaskUsersTableSQL = `
CREATE TABLE IF NOT EXISTS privacy_mask_users (
  tenant_id TEXT NOT NULL,
  end_user_id TEXT NOT NULL,
  policy_json TEXT NOT NULL DEFAULT '',
  allowed INTEGER,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, end_user_id)
);
`

// MaskUser is what is stored for one portal user: their own policy, nil when
// they have none, and the tenant's override of whether they may keep one, nil
// when the tenant's default applies.
type MaskUser struct {
	Policy    *MaskPolicy `json:"policy,omitempty"`
	Allowed   *bool       `json:"allowed,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
}

// applies is the user's own policy when the tenant lets it apply, nil when the
// tenant's default applies to the user's requests.
func (u MaskUser) applies(tenant SecretMaskingSettings) *MaskPolicy {
	if u.Policy == nil || !u.allowed(tenant) {
		return nil
	}
	return u.Policy
}

func (u MaskUser) allowed(tenant SecretMaskingSettings) bool {
	if u.Allowed != nil {
		return *u.Allowed
	}
	return tenant.PortalAllowed()
}

// Allowed says whether the user may keep a policy of their own, under the
// tenant's settings.
func (u MaskUser) AllowedUnder(tenant SecretMaskingSettings) bool { return u.allowed(tenant) }

func scanMaskUser(policyJSON string, allowed sql.NullInt64, updatedAt string) (MaskUser, error) {
	user := MaskUser{UpdatedAt: updatedAt}
	if strings.TrimSpace(policyJSON) != "" {
		var policy MaskPolicy
		if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
			return MaskUser{}, err
		}
		user.Policy = &policy
	}
	if allowed.Valid {
		on := allowed.Int64 != 0
		user.Allowed = &on
	}
	return user, nil
}

// GetMaskUser returns what is stored for a portal user, the zero MaskUser when
// nothing is.
func (s *Store) GetMaskUser(ctx context.Context, tenantID, endUserID string) (MaskUser, error) {
	if s == nil || s.db == nil {
		return MaskUser{}, ErrUnavailable
	}
	var policyJSON, updatedAt string
	var allowed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT policy_json, allowed, updated_at FROM privacy_mask_users WHERE tenant_id=$1 AND end_user_id=$2`,
		strings.TrimSpace(tenantID), strings.TrimSpace(endUserID)).Scan(&policyJSON, &allowed, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MaskUser{}, nil
	}
	if err != nil {
		return MaskUser{}, err
	}
	return scanMaskUser(policyJSON, allowed, updatedAt)
}

// ListMaskUsers returns what is stored for every portal user of a tenant who
// has a row, keyed by user id; with tenantID "" those of every tenant, keyed
// by tenant id and user id joined by a zero byte.
func (s *Store) ListMaskUsers(ctx context.Context, tenantID string) (map[string]MaskUser, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	query, args := `SELECT tenant_id, end_user_id, policy_json, allowed, updated_at FROM privacy_mask_users`, []any{}
	if tenantID != "" {
		query += ` WHERE tenant_id=$1`
		args = append(args, strings.TrimSpace(tenantID))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MaskUser{}
	for rows.Next() {
		var tenant, endUser, policyJSON, updatedAt string
		var allowed sql.NullInt64
		if err := rows.Scan(&tenant, &endUser, &policyJSON, &allowed, &updatedAt); err != nil {
			return nil, err
		}
		user, err := scanMaskUser(policyJSON, allowed, updatedAt)
		if err != nil {
			continue // one row written by hand must not hide the others
		}
		if tenantID == "" {
			endUser = tenant + "\x00" + endUser
		}
		out[endUser] = user
	}
	return out, rows.Err()
}

// PutMaskUserPolicy saves a portal user's own policy (normalized already), or
// with nil removes it, keeping the tenant's override.
func (s *Store) PutMaskUserPolicy(ctx context.Context, tenantID, endUserID string, policy *MaskPolicy) error {
	payload := ""
	if policy != nil {
		raw, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		payload = string(raw)
	}
	return s.putMaskUser(ctx, tenantID, endUserID, "policy_json", payload)
}

// SetMaskUserAllowed sets a portal user's override, or with nil removes it so
// that the tenant's default applies again.
func (s *Store) SetMaskUserAllowed(ctx context.Context, tenantID, endUserID string, allowed *bool) error {
	var value any
	if allowed != nil {
		value = boolColumn(*allowed)
	}
	return s.putMaskUser(ctx, tenantID, endUserID, "allowed", value)
}

func (s *Store) putMaskUser(ctx context.Context, tenantID, endUserID, column string, value any) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tenantID, endUserID = strings.TrimSpace(tenantID), strings.TrimSpace(endUserID)
	if endUserID == "" {
		return errors.New("a portal user id is required")
	}
	// column is one of two names written above, never a client's
	_, err := s.db.ExecContext(ctx, `INSERT INTO privacy_mask_users (tenant_id,end_user_id,`+column+`,updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT(tenant_id,end_user_id) DO UPDATE SET `+column+`=excluded.`+column+`,updated_at=excluded.updated_at`,
		tenantID, endUserID, value, formatTime(time.Now().UTC()))
	if err == nil {
		MaskUsers().forget(tenantID, endUserID)
	}
	return err
}

// DeleteMaskUser removes what is stored for a portal user who has been
// deleted, and their records.
func (s *Store) DeleteMaskUser(ctx context.Context, tenantID, endUserID string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tenantID, endUserID = strings.TrimSpace(tenantID), strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	for _, table := range []string{"privacy_mask_users", "privacy_mask_records", "privacy_mask_value_days", "privacy_mask_days"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id=$1 AND end_user_id=$2`, tenantID, endUserID); err != nil {
			return err
		}
	}
	MaskUsers().forget(tenantID, endUserID)
	return nil
}

// ---- The cache the request path reads ----

type maskUserKey struct{ tenantID, endUserID string }

type maskUserEntry struct {
	user  MaskUser
	until time.Time
	read  bool
}

// MaskUserRegistry caches the portal users' masking rows for the request path,
// as SecretMaskingRegistry does the tenants' settings and with the same waits.
// A write made through this process replaces the entry at once; the TTL bounds
// how long one made by another process goes unseen.
type MaskUserRegistry struct {
	mu        sync.RWMutex
	store     *Store
	entries   map[maskUserKey]maskUserEntry
	gen       uint64
	preloaded bool
	loads     singleflight.Group
	now       func() time.Time
}

var defaultMaskUsers = &MaskUserRegistry{entries: map[maskUserKey]maskUserEntry{}, now: time.Now}

// MaskUsers returns the registry of this process.
func MaskUsers() *MaskUserRegistry { return defaultMaskUsers }

// maxMaskUserEntries bounds the cache; when it is reached it starts over.
const maxMaskUserEntries = 16384

// SetStore attaches the store and reads every row in the background.
func (r *MaskUserRegistry) SetStore(store *Store) {
	r.mu.Lock()
	r.store, r.entries, r.preloaded = store, map[maskUserKey]maskUserEntry{}, false
	r.gen++
	r.mu.Unlock()
	if store == nil {
		return
	}
	go func() {
		for attempt := 0; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*secretMaskingLoadTimeout)
			all, err := store.ListMaskUsers(ctx, "")
			cancel()
			if err == nil {
				r.preload(store, all)
				return
			}
			time.Sleep(secretMaskingPreloadRetry)
			r.mu.RLock()
			same := r.store == store
			r.mu.RUnlock()
			if !same {
				return
			}
		}
	}()
}

func (r *MaskUserRegistry) preload(store *Store, all map[string]MaskUser) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != store {
		return
	}
	now := r.now()
	for joined, user := range all {
		tenantID, endUserID, _ := strings.Cut(joined, "\x00")
		key := maskUserKey{tenantID, endUserID}
		if have, ok := r.entries[key]; ok && have.read {
			continue
		}
		r.entries[key] = maskUserEntry{user: user, until: now.Add(secretMaskingTTL), read: true}
	}
	r.preloaded = true
}

// forget drops a user's entry after a write, and what masking remembers of
// their requests: their next request reads the row again.
func (r *MaskUserRegistry) forget(tenantID, endUserID string) {
	r.mu.Lock()
	delete(r.entries, maskUserKey{tenantID, endUserID})
	r.gen++
	r.mu.Unlock()
	maskScopes.Drop(tenantID + "\x00u:" + endUserID)
}

// Lookup returns a portal user's row; known is false when it has never been
// read and could not be read now (see SecretMaskingRegistry.Lookup).
func (r *MaskUserRegistry) Lookup(tenantID, endUserID string) (user MaskUser, known bool) {
	r.mu.RLock()
	store, cached, gen, preloaded := r.store, r.entries[maskUserKey{tenantID, endUserID}], r.gen, r.preloaded
	r.mu.RUnlock()
	if store == nil {
		return MaskUser{}, true
	}
	if !cached.read && preloaded {
		cached.read = true // no row when every row was read
	}
	if r.now().Before(cached.until) {
		return cached.user, true
	}
	loaded := r.loads.DoChan(tenantID+"\x00"+endUserID, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), secretMaskingLoadTimeout)
		defer cancel()
		got, err := store.GetMaskUser(ctx, tenantID, endUserID)
		now := r.now()
		next := maskUserEntry{user: got, until: now.Add(secretMaskingTTL), read: true}
		if err != nil {
			next = maskUserEntry{user: cached.user, until: now.Add(secretMaskingRetryAfterFailure), read: cached.read}
		}
		r.mu.Lock()
		if r.gen == gen {
			if len(r.entries) >= maxMaskUserEntries {
				r.entries = map[maskUserKey]maskUserEntry{}
			}
			r.entries[maskUserKey{tenantID, endUserID}] = next
		}
		r.mu.Unlock()
		return next, nil
	})
	waitFor := secretMaskingRefreshWait
	if !cached.read {
		waitFor = secretMaskingFirstReadWait
	}
	wait := time.NewTimer(waitFor)
	defer wait.Stop()
	select {
	case result := <-loaded:
		entry := result.Val.(maskUserEntry)
		return entry.user, entry.read
	case <-wait.C:
		return cached.user, cached.read
	}
}
