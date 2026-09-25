package clusterauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// importLockWait bounds how long a starting node waits for another node's
// import to finish before giving up on startup.
const importLockWait = 2 * time.Minute

// The statements below run through the pgxq driver, which rewrites '?' and a
// few SQLite type names; they use $n placeholders and avoid both.
const credentialColumns = `id, tenant_id, file_name, auth_index, provider, content, runtime, version,
	deleted_at IS NOT NULL, created_at, updated_at`

type pgBackend struct {
	db     *sql.DB
	ownsDB bool
}

func newPGBackend(db *sql.DB, ownsDB bool) *pgBackend {
	return &pgBackend{db: db, ownsDB: ownsDB}
}

func (b *pgBackend) close() error {
	if b.ownsDB && b.db != nil {
		return b.db.Close()
	}
	return nil
}

func (b *pgBackend) withImportLock(ctx context.Context, fn func(context.Context) error) error {
	return cluster.WithSessionLock(ctx, b.db, cluster.LockAuthImport, importLockWait, fn)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(scanner rowScanner) (row, error) {
	var r row
	var content, runtime []byte
	if err := scanner.Scan(&r.ID, &r.TenantID, &r.FileName, &r.AuthIndex, &r.Provider, &content, &runtime,
		&r.Version, &r.Deleted, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return row{}, err
	}
	r.Content = append([]byte(nil), content...)
	r.Runtime = append([]byte(nil), runtime...)
	return r, nil
}

func (b *pgBackend) queryRows(ctx context.Context, query string, args ...any) ([]row, error) {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]row, 0, 32)
	for rows.Next() {
		r, errScan := scanRow(rows)
		if errScan != nil {
			return nil, errScan
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (b *pgBackend) count(ctx context.Context) (int, error) {
	var n int
	err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM auth_credentials`).Scan(&n)
	return n, err
}

func (b *pgBackend) listLive(ctx context.Context) ([]row, error) {
	return b.queryRows(ctx, `SELECT `+credentialColumns+` FROM auth_credentials WHERE deleted_at IS NULL ORDER BY id`)
}

func (b *pgBackend) listVersions(ctx context.Context) ([]rowVersion, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, version, deleted_at IS NOT NULL FROM auth_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]rowVersion, 0, 32)
	for rows.Next() {
		var v rowVersion
		if err = rows.Scan(&v.ID, &v.Version, &v.Deleted); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (b *pgBackend) get(ctx context.Context, id string) (*row, error) {
	r, err := scanRow(b.db.QueryRowContext(ctx, `SELECT `+credentialColumns+` FROM auth_credentials WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// jsonList encodes a string list as a JSON array parameter. The pgxq driver
// wrapper does not forward pgx's value checker, so database/sql rejects Go
// slices; the statements unpack the array server-side instead.
func jsonList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	raw, err := json.Marshal(values)
	return string(raw), err
}

func (b *pgBackend) getMany(ctx context.Context, ids []string) ([]row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	return b.queryRows(ctx, `SELECT `+credentialColumns+` FROM auth_credentials
		WHERE id IN (SELECT jsonb_array_elements_text($1::jsonb))`, list)
}

// inTx runs fn in a transaction; the notification fn queues is delivered only
// if the transaction commits.
func (b *pgBackend) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (b *pgBackend) upsert(ctx context.Context, r row, publish publishFunc) (row, error) {
	stored := row{ID: r.ID}
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO auth_credentials (id, tenant_id, file_name, auth_index, provider, content, runtime,
			                              version, created_at, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, 1, now(), now(), $8)
			ON CONFLICT (id) DO UPDATE SET
			  tenant_id = excluded.tenant_id,
			  file_name = excluded.file_name,
			  provider = excluded.provider,
			  content = excluded.content,
			  runtime = excluded.runtime,
			  version = auth_credentials.version + 1,
			  deleted_at = NULL,
			  updated_at = now(),
			  updated_by = excluded.updated_by,
			  auth_index = CASE WHEN auth_credentials.auth_index = '' THEN excluded.auth_index
			                    ELSE auth_credentials.auth_index END
			RETURNING version, auth_index, created_at, updated_at`,
			r.ID, r.TenantID, r.FileName, r.AuthIndex, r.Provider, string(r.Content), string(r.Runtime), r.UpdatedBy,
		).Scan(&stored.Version, &stored.AuthIndex, &stored.CreatedAt, &stored.UpdatedAt); err != nil {
			return fmt.Errorf("upsert credential %s: %w", r.ID, err)
		}
		return publish(tx, stored.Version)
	})
	return stored, err
}

func (b *pgBackend) compareAndSwap(ctx context.Context, id string, expected int64, r row, publish publishFunc) (int64, bool, error) {
	var version int64
	applied := false
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE auth_credentials
			   SET content = $3::jsonb, file_name = $4, provider = $5,
			       version = version + 1, updated_at = now(), updated_by = $6
			 WHERE id = $1 AND version = $2 AND deleted_at IS NULL
			RETURNING version`,
			id, expected, string(r.Content), r.FileName, r.Provider, r.UpdatedBy,
		).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("update credential %s: %w", id, err)
		}
		applied = true
		return publish(tx, version)
	})
	return version, applied && err == nil, err
}

func (b *pgBackend) tombstone(ctx context.Context, id, updatedBy string, publish publishFunc) (int64, bool, error) {
	var version int64
	applied := false
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		// The secret material goes with the credential; the row stays so the
		// deletion outranks late writes and the version keeps increasing.
		err := tx.QueryRowContext(ctx, `
			UPDATE auth_credentials
			   SET deleted_at = now(), version = version + 1,
			       content = '{}'::jsonb, runtime = '{}'::jsonb,
			       refresh_owner = NULL, refresh_until = NULL,
			       updated_at = now(), updated_by = $2
			 WHERE id = $1 AND deleted_at IS NULL
			RETURNING version`, id, updatedBy).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("delete credential %s: %w", id, err)
		}
		applied = true
		return publish(tx, version)
	})
	return version, applied && err == nil, err
}

func (b *pgBackend) patchRuntime(ctx context.Context, id, updatedBy string, set map[string]json.RawMessage, removed []string) error {
	patch, err := json.Marshal(set)
	if err != nil {
		return err
	}
	drop, err := jsonList(removed)
	if err != nil {
		return err
	}
	_, err = b.db.ExecContext(ctx, `
		UPDATE auth_credentials
		   SET runtime = (runtime - ARRAY(SELECT jsonb_array_elements_text($2::jsonb))) || $3::jsonb,
		       updated_at = now(), updated_by = $4
		 WHERE id = $1 AND deleted_at IS NULL`, id, drop, string(patch), updatedBy)
	return err
}

func (b *pgBackend) claimLease(ctx context.Context, id, owner string, ttl time.Duration) (*row, error) {
	r, err := scanRow(b.db.QueryRowContext(ctx, `
		UPDATE auth_credentials
		   SET refresh_owner = $2, refresh_until = now() + make_interval(secs => $3)
		 WHERE id = $1 AND deleted_at IS NULL
		   AND (refresh_until IS NULL OR refresh_until < now() OR refresh_owner = $2)
		RETURNING `+credentialColumns, id, owner, ttl.Seconds()))
	if err == nil {
		return &r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var deleted bool
	err = b.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM auth_credentials WHERE id = $1`, id).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deleted) {
		return nil, coreauth.ErrCredentialGone
	}
	return nil, err
}

func (b *pgBackend) releaseLease(ctx context.Context, id, owner string) error {
	_, err := b.db.ExecContext(ctx, `
		UPDATE auth_credentials SET refresh_owner = NULL, refresh_until = NULL
		 WHERE id = $1 AND refresh_owner = $2`, id, owner)
	return err
}

func (b *pgBackend) insertImported(ctx context.Context, rows []row) (int, error) {
	inserted := 0
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO auth_credentials (id, tenant_id, file_name, auth_index, provider, content, runtime,
				                              version, created_at, updated_at, updated_by)
				VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, 1, $8, $8, $9)
				ON CONFLICT (id) DO NOTHING`,
				r.ID, r.TenantID, r.FileName, r.AuthIndex, r.Provider, string(r.Content), string(r.Runtime), r.UpdatedAt, r.UpdatedBy)
			if err != nil {
				return fmt.Errorf("import credential %s: %w", r.ID, err)
			}
			if n, errRows := result.RowsAffected(); errRows == nil {
				inserted += int(n)
			}
		}
		return nil
	})
	return inserted, err
}

func (b *pgBackend) activeBindingIndexes(ctx context.Context) (map[string]string, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT tenant_id::text, auth_id, auth_index FROM ai_account_tenant_bindings
		 WHERE binding_state = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var tenantID, authID, authIndex string
		if err = rows.Scan(&tenantID, &authID, &authIndex); err != nil {
			return nil, err
		}
		out[bindingKey(tenantID, authID)] = authIndex
	}
	return out, rows.Err()
}
