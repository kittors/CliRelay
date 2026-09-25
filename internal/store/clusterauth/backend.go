package clusterauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// row is one credential as stored in auth_credentials.
type row struct {
	ID        string
	TenantID  string
	FileName  string
	AuthIndex string
	Provider  string
	// Content is the credential document without runtime keys, a JSON object.
	Content []byte
	// Runtime holds the runtime observations, a JSON object.
	Runtime   []byte
	Version   int64
	Deleted   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// UpdatedBy names the node performing a write; unused on reads.
	UpdatedBy string
}

// rowVersion is the per-row summary the reconcile loop compares.
type rowVersion struct {
	ID      string
	Version int64
	Deleted bool
}

// publishFunc queues the change notification inside the write transaction,
// so peers only hear about versions that committed. tx is nil when the
// backend has no transactions.
type publishFunc func(tx *sql.Tx, version int64) error

// backend is the persistence the store needs. PostgreSQL is the production
// implementation; tests drive the same store logic with an in-memory one.
type backend interface {
	// withImportLock runs fn while no other node can run its import.
	withImportLock(ctx context.Context, fn func(context.Context) error) error
	// count returns the number of rows, tombstones included.
	count(ctx context.Context) (int, error)
	listLive(ctx context.Context) ([]row, error)
	listVersions(ctx context.Context) ([]rowVersion, error)
	// get returns the row id, tombstones included, or nil when absent.
	get(ctx context.Context, id string) (*row, error)
	getMany(ctx context.Context, ids []string) ([]row, error)
	// upsert creates id or overwrites it whatever its state, reviving a
	// tombstone. The version keeps increasing and an auth_index already
	// assigned is kept. It returns the stored row without content.
	upsert(ctx context.Context, r row, publish publishFunc) (row, error)
	// compareAndSwap replaces the content of the live row id if its version
	// is expected; ok is false otherwise.
	compareAndSwap(ctx context.Context, id string, expected int64, r row, publish publishFunc) (version int64, ok bool, err error)
	// tombstone deletes the live row id; ok is false when there was none.
	tombstone(ctx context.Context, id, updatedBy string, publish publishFunc) (version int64, ok bool, err error)
	// patchRuntime merges set into, and drops removed from, the runtime of
	// the live row id. It never touches a tombstone.
	patchRuntime(ctx context.Context, id, updatedBy string, set map[string]json.RawMessage, removed []string) error
	// claimLease takes the refresh lease of id for owner. It returns the row
	// when acquired, nil when another owner holds the lease, and
	// coreauth.ErrCredentialGone when the credential does not exist.
	claimLease(ctx context.Context, id, owner string, ttl time.Duration) (*row, error)
	releaseLease(ctx context.Context, id, owner string) error
	// insertImported adds rows that do not exist yet and reports how many.
	insertImported(ctx context.Context, rows []row) (int, error)
	// activeBindingIndexes returns the auth_index of every active account
	// binding keyed by bindingKey(tenant, auth id).
	activeBindingIndexes(ctx context.Context) (map[string]string, error)
	close() error
}

// bindingKey keys account bindings by tenant and auth ID. PostgreSQL prints
// UUIDs in lower case; tenant directory names are compared the same way.
func bindingKey(tenantID, authID string) string {
	return strings.ToLower(strings.TrimSpace(tenantID)) + "\x00" + authID
}
