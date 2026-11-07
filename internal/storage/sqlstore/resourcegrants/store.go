// Package resourcegrants stores which tenants may use a resource that the
// system tenant owns: a proxy of its proxy pool, or one of its AI account
// credentials.
//
// Sharing is a grant, never a copy. A shared credential stays one row with one
// refresh token: copies held by two tenants refresh independently, and OpenAI
// and Anthropic rotate refresh tokens on every use, so each copy would revoke
// the other's (refresh_token_reused). A shared proxy is likewise one entry the
// platform edits once for every tenant that uses it.
//
// Only the system tenant owns shared resources. The table therefore records
// grantees only; the owner is implied.
package resourcegrants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	log "github.com/sirupsen/logrus"
)

// Resource kinds. The values are stored, so they must never be renamed.
const (
	KindProxy = "proxy"
	KindAuth  = "auth"
)

// AllTenants grants a resource to every tenant, including tenants created
// after the grant.
const AllTenants = "*"

// createTableSQL mirrors the PostgreSQL migration for SQLite test databases.
const createTableSQL = `
CREATE TABLE IF NOT EXISTS platform_resource_grants (
  resource_kind TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  tenant_id     TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (resource_kind, resource_id, tenant_id)
);
`

// Store reads and writes platform_resource_grants.
type Store struct {
	db *sql.DB
}

// NewStore returns a store over db; a nil db yields an unavailable store.
func NewStore(db *sql.DB) Store {
	return Store{db: db}
}

// InitTable creates the table when no migration did (SQLite test databases).
// On PostgreSQL the versioned migration owns it and this is a no-op.
func InitTable(db *sql.DB) {
	if db == nil {
		return
	}
	configsync.InitTables(db)
	if _, err := db.Exec(createTableSQL); err != nil {
		log.Errorf("sqlstore/resourcegrants: create platform_resource_grants table: %v", err)
	}
}

// Available reports whether the store has a database.
func (s Store) Available() bool {
	return s.db != nil
}

// ValidKind reports whether kind names a shareable resource.
func ValidKind(kind string) bool {
	return kind == KindProxy || kind == KindAuth
}

// NormalizeTenants trims, deduplicates and sorts grantee tenant IDs. AllTenants
// absorbs every other entry: a resource shared with everybody needs no list.
func NormalizeTenants(tenants []string) []string {
	seen := make(map[string]struct{}, len(tenants))
	out := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		tenant = strings.TrimSpace(tenant)
		if tenant == "" {
			continue
		}
		if tenant == AllTenants {
			return []string{AllTenants}
		}
		if _, ok := seen[tenant]; ok {
			continue
		}
		seen[tenant] = struct{}{}
		out = append(out, tenant)
	}
	slices.Sort(out)
	return out
}

// Allows reports whether grantees include tenantID, directly or through
// AllTenants.
func Allows(grantees []string, tenantID string) bool {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return false
	}
	for _, grantee := range grantees {
		if grantee == AllTenants || grantee == tenantID {
			return true
		}
	}
	return false
}

// List returns the grantees of every granted resource of kind, keyed by
// resource ID. Resources shared with nobody are absent.
func (s Store) List(ctx context.Context, kind string) (map[string][]string, error) {
	if s.db == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT resource_id, tenant_id FROM platform_resource_grants
		WHERE resource_kind = ? ORDER BY resource_id ASC, tenant_id ASC`, kind)
	if err != nil {
		return nil, fmt.Errorf("list platform_resource_grants: %w", err)
	}
	defer rows.Close()
	grants := make(map[string][]string)
	for rows.Next() {
		var resourceID, tenantID string
		if err := rows.Scan(&resourceID, &tenantID); err != nil {
			return nil, fmt.Errorf("scan platform_resource_grants row: %w", err)
		}
		grants[resourceID] = append(grants[resourceID], tenantID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read platform_resource_grants rows: %w", err)
	}
	for id, tenants := range grants {
		grants[id] = NormalizeTenants(tenants)
	}
	return grants, nil
}

// Replace gives every resource in ids exactly the grantees in tenants; an empty
// tenants list stops sharing them. The write bumps the grants collection
// version and announces it, so every node reloads.
func (s Store) Replace(ctx context.Context, kind string, ids []string, tenants []string) (int64, error) {
	if s.db == nil {
		return 0, errors.New("database not initialised")
	}
	if !ValidKind(kind) {
		return 0, fmt.Errorf("unknown resource kind %q", kind)
	}
	ids = normalizeIDs(ids)
	if len(ids) == 0 {
		return 0, errors.New("no resource ids")
	}
	tenants = NormalizeTenants(tenants)
	return configsync.WriteCollection(ctx, s.db, configsync.DomainResourceGrants, "", configsync.AnyVersion, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339)
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM platform_resource_grants WHERE resource_kind = ? AND resource_id = ?`, kind, id); err != nil {
				return err
			}
			for _, tenant := range tenants {
				if _, err := tx.ExecContext(ctx, `INSERT INTO platform_resource_grants (resource_kind, resource_id, tenant_id, created_at)
					VALUES (?, ?, ?, ?)`, kind, id, tenant, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RetainTx deletes, inside tx, the grants of kind whose resource is not in
// keep, and bumps the grants collection version when it deleted any. Owners
// call it when they replace their resource list: a grant must not outlive its
// resource, or a resource created later under the same ID would be shared
// without anybody deciding so. It reports whether grants changed; the caller
// announces configsync.DomainResourceGrants with its own write.
func RetainTx(ctx context.Context, tx *sql.Tx, kind string, keep []string) (bool, error) {
	if tx == nil {
		return false, errors.New("nil transaction")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT resource_id FROM platform_resource_grants WHERE resource_kind = ?`, kind)
	if err != nil {
		return false, fmt.Errorf("list platform_resource_grants: %w", err)
	}
	kept := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		kept[id] = struct{}{}
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan platform_resource_grants row: %w", err)
		}
		if _, ok := kept[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(stale) == 0 {
		return false, nil
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM platform_resource_grants WHERE resource_kind = ? AND resource_id = ?`, kind, id); err != nil {
			return false, err
		}
	}
	if _, err := configsync.BumpTx(ctx, tx, configsync.DomainResourceGrants, "", configsync.AnyVersion); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
