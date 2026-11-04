package contentmoderation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// A portal user (an end user of a tenant, who signs in to the portal) may keep a
// word policy of their own (WordScopeEndUser) when the tenant lets them. The
// tenant decides in two steps: a default for all of its portal users, and an
// override for one user that allows or forbids whatever the default says. A
// tenant that has decided nothing allows no one.
//
// The permission is checked when a user saves a policy and again on every
// request. A user the tenant stops allowing keeps the policy, which applies
// again if the tenant allows it again, but it stops applying at once: the
// portal no longer shows it to the user, who could not switch it off either.

// wordPortalTablesSQL holds the permissions, one row per decision: the
// tenant's default under the empty user id, and the overrides of single users.
// Like the policy table it only adds a table, so an older build keeps running
// on the upgraded database.
const wordPortalTablesSQL = `
CREATE TABLE IF NOT EXISTS content_moderation_word_portal_access (
  tenant_id TEXT NOT NULL,
  end_user_id TEXT NOT NULL DEFAULT '',
  allowed INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, end_user_id)
);
`

// WordPortalAccess is a tenant's decision about its portal users' word
// policies.
type WordPortalAccess struct {
	// DefaultAllowed applies to every portal user without an override.
	DefaultAllowed bool `json:"default_allowed"`
	// Overrides maps the id of a portal user to the user's own permission.
	Overrides map[string]bool `json:"overrides"`
}

// Allowed reports whether the portal user endUserID may keep a word policy.
func (a WordPortalAccess) Allowed(endUserID string) bool {
	if allowed, ok := a.Overrides[strings.TrimSpace(endUserID)]; ok {
		return allowed
	}
	return a.DefaultAllowed
}

// wordPortalGrant gathers the rows read for one portal user: the tenant's
// default and the user's override, either of which may be missing.
type wordPortalGrant struct {
	hasDefault, defaultAllowed   bool
	hasOverride, overrideAllowed bool
}

func (g *wordPortalGrant) add(endUserID string, allowed bool) {
	if endUserID == "" {
		g.hasDefault, g.defaultAllowed = true, allowed
		return
	}
	g.hasOverride, g.overrideAllowed = true, allowed
}

func (g wordPortalGrant) allowed() bool {
	if g.hasOverride {
		return g.overrideAllowed
	}
	return g.hasDefault && g.defaultAllowed
}

// maxRememberedPortalGrants bounds wordPortalCache; when it is reached the
// cache starts over, as the policy cache does.
const maxRememberedPortalGrants = 8192

type wordPortalKey struct{ tenantID, endUserID string }

// wordPortalCache remembers whether each portal user was last found allowed a
// policy, for the requests that go on without the database (see
// rememberedWordPolicies).
type wordPortalCache struct {
	mu    sync.Mutex
	items map[wordPortalKey]bool
}

func (c *wordPortalCache) allowed(tenantID, endUserID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[wordPortalKey{tenantID, endUserID}]
}

func (c *wordPortalCache) put(tenantID, endUserID string, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil || len(c.items) >= maxRememberedPortalGrants {
		c.items = make(map[wordPortalKey]bool)
	}
	c.items[wordPortalKey{tenantID, endUserID}] = allowed
}

// GetWordPortalAccess returns what a tenant has decided about its portal users'
// word policies.
func (s *Store) GetWordPortalAccess(ctx context.Context, tenantID string) (WordPortalAccess, error) {
	if s == nil || s.db == nil {
		return WordPortalAccess{}, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT end_user_id, allowed FROM content_moderation_word_portal_access WHERE tenant_id=$1`, strings.TrimSpace(tenantID))
	if err != nil {
		return WordPortalAccess{}, err
	}
	defer rows.Close()
	access := WordPortalAccess{Overrides: map[string]bool{}}
	for rows.Next() {
		var endUserID string
		var allowed int
		if err := rows.Scan(&endUserID, &allowed); err != nil {
			return WordPortalAccess{}, err
		}
		if endUserID == "" {
			access.DefaultAllowed = allowed != 0
		} else {
			access.Overrides[endUserID] = allowed != 0
		}
	}
	return access, rows.Err()
}

// WordPortalAllowed reports whether a tenant allows one portal user a word
// policy.
func (s *Store) WordPortalAllowed(ctx context.Context, tenantID, endUserID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, ErrUnavailable
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT end_user_id, allowed FROM content_moderation_word_portal_access WHERE tenant_id=$1 AND (end_user_id='' OR end_user_id=$2)`,
		strings.TrimSpace(tenantID), endUserID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var grant wordPortalGrant
	for rows.Next() {
		var id string
		var allowed int
		if err := rows.Scan(&id, &allowed); err != nil {
			return false, err
		}
		grant.add(id, allowed != 0)
	}
	return grant.allowed(), rows.Err()
}

// SetWordPortalDefault sets whether a tenant's portal users without an override
// may keep a word policy.
func (s *Store) SetWordPortalDefault(ctx context.Context, tenantID string, allowed bool) error {
	return s.putWordPortalRow(ctx, tenantID, "", allowed)
}

// SetWordPortalOverride sets one portal user's permission, whatever the
// tenant's default says, or with a nil allowed removes it so that the default
// applies again.
func (s *Store) SetWordPortalOverride(ctx context.Context, tenantID, endUserID string, allowed *bool) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return errors.New("a portal user id is required")
	}
	if allowed != nil {
		return s.putWordPortalRow(ctx, tenantID, endUserID, *allowed)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM content_moderation_word_portal_access WHERE tenant_id=$1 AND end_user_id=$2`,
		strings.TrimSpace(tenantID), endUserID)
	return err
}

func (s *Store) putWordPortalRow(ctx context.Context, tenantID, endUserID string, allowed bool) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO content_moderation_word_portal_access (tenant_id,end_user_id,allowed,updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT(tenant_id,end_user_id) DO UPDATE SET allowed=excluded.allowed,updated_at=excluded.updated_at`,
		strings.TrimSpace(tenantID), endUserID, boolColumn(allowed), formatTime(time.Now().UTC()))
	return err
}

// DeleteEndUserWordData removes what is stored for a portal user who has been
// deleted: the user's policy and the tenant's override for the user. Ids are
// not reused, but a user restored under the same id should start from the
// tenant's default, not from what the deleted one had.
func (s *Store) DeleteEndUserWordData(ctx context.Context, tenantID, endUserID string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tenantID, endUserID = strings.TrimSpace(tenantID), strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_moderation_word_policies WHERE tenant_id=$1 AND scope_type=$2 AND scope_id=$3`,
		tenantID, WordScopeEndUser, endUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_moderation_word_portal_access WHERE tenant_id=$1 AND end_user_id=$2`,
		tenantID, endUserID); err != nil {
		return err
	}
	return tx.Commit()
}
