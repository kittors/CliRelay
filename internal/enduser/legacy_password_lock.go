package enduser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// legacyBackfillPassword is the initial password the API key backfill wrote
// into every account it created in v0.4.13 through v0.5.1 (v0.5.2's password
// policy made it fail instead, and #1040 replaced it), with
// must_change_password left false. It was in the published source, and the
// usernames beside it are derived from key names (see lockedAccountHash).
//
// It is kept for one purpose only: recognising the accounts that still hold it,
// so LockLegacyBackfillPasswordAccounts can lock them. Nothing may hash it into
// an account again.
const legacyBackfillPassword = "password123"

// neverChangedSlack bounds how far apart two timestamps the backfill took from
// one transaction clock may read: an account's created_at against the run's
// done_at, and its password_changed_at against its created_at. Both pairs are
// equal as written; the slack only absorbs storage precision. The bcrypt
// comparison, not this window, is what decides whether an account is locked.
const neverChangedSlack = time.Second

// holdsLegacyBackfillPassword is a variable so tests can count the comparisons
// the pass makes: each is a bcrypt verification paid at boot, before listening.
var holdsLegacyBackfillPassword = func(hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(legacyBackfillPassword)) == nil
}

type legacyPasswordCandidate struct {
	id, username, passwordHash string
}

// LockLegacyBackfillPasswordAccounts locks the portal accounts still holding
// legacyBackfillPassword and returns their usernames.
//
// #1040 stopped the backfill minting such accounts but left the ones it had
// already created as they were: a portal login on the published password, with
// the account's keys behind it. This one-shot pass gives each of them what the
// backfill now gives a new account — a random password whose plaintext is
// discarded, and must_change_password set — and revokes its sessions. The
// account stays visible and keeps its keys, but cannot be signed into until an
// admin issues a credential through the end-user password reset. Accounts whose
// owner already replaced the password are not touched.
//
// Only the backfill's own batch is examined (see loadLegacyPasswordCandidates).
// An account outside it is out of scope even if its password happens to be the
// same string: an admin or its owner set that by hand, under the 8-character
// policy the current 12-character one no longer accepts. This pass retires the
// credential the backfill published, not weak passwords in general. It also
// bounds boot time: accounts outside the batch cost nothing, the batch shares
// one hash so it costs one bcrypt comparison, and a database the backfill never
// ran on costs none.
//
// Deliberately left alone: status, because API key admission reads it and never
// the password or must_change_password, so traffic through the owners' keys
// continues; and password_changed_at, because nobody chose a new password.
//
// Runs once: end_user_legacy_password_lock_state records completion, and the
// backfill's advisory-lock pattern serialises concurrent boots (blue-green,
// replicas).
func (s *Service) LockLegacyBackfillPasswordAccounts(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// A key of its own rather than the backfill's 74201501. Fail closed on
	// Postgres; only ignore engines that do not implement advisory locks.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(74201502)`); err != nil {
		if !isUnsupportedAdvisoryLockError(err) {
			return nil, fmt.Errorf("enduser legacy password lock: %w", err)
		}
	}
	var done int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM end_user_legacy_password_lock_state WHERE id = 1`).Scan(&done); err != nil {
		// Missing table after migrations is a hard error; never skip blindly.
		return nil, err
	}
	if done > 0 {
		return nil, nil
	}

	locked := make([]string, 0)
	batchAt, backfillRan, err := legacyBackfillBatchTime(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Without a recorded run no account here is one the backfill created, so
	// there is nothing to compare; the pass just records itself.
	if backfillRan {
		if locked, err = lockLegacyBackfillBatchTx(ctx, tx, batchAt); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO end_user_legacy_password_lock_state (id, done_at, locked_count)
		VALUES (1, CURRENT_TIMESTAMP, ?) ON CONFLICT (id) DO NOTHING
	`, len(locked)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return locked, nil
}

// legacyBackfillBatchTime returns end_user_backfill_state.done_at, which is
// also the created_at of every account that backfill run wrote; ok is false
// when the backfill never ran here. A missing table is an error, as in the
// backfill.
func legacyBackfillBatchTime(ctx context.Context, tx *sql.Tx) (batchAt time.Time, ok bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT done_at FROM end_user_backfill_state WHERE id = 1`).Scan(&batchAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return batchAt, true, nil
}

// lockLegacyBackfillBatchTx locks the accounts of the backfill batch that still
// verify against legacyBackfillPassword.
func lockLegacyBackfillBatchTx(ctx context.Context, tx *sql.Tx, batchAt time.Time) ([]string, error) {
	candidates, err := loadLegacyPasswordCandidates(ctx, tx, batchAt)
	if err != nil {
		return nil, err
	}
	// The run hashed once and gave every account that same string, so a verdict
	// per distinct hash makes the batch one comparison rather than one per
	// account. The replacement is hashed on the first match, so a batch with
	// nothing left to lock does no hashing either.
	verdicts := make(map[string]bool)
	replacement := ""
	locked := make([]string, 0)
	for _, candidate := range candidates {
		holds, seen := verdicts[candidate.passwordHash]
		if !seen {
			holds = holdsLegacyBackfillPassword(candidate.passwordHash)
			verdicts[candidate.passwordHash] = holds
		}
		if !holds {
			continue
		}
		if replacement == "" {
			if replacement, err = lockedAccountHash(); err != nil {
				return nil, err
			}
		}
		lockedIt, lockErr := lockLegacyPasswordAccountTx(ctx, tx, candidate, replacement)
		if lockErr != nil {
			return nil, lockErr
		}
		if lockedIt {
			locked = append(locked, candidate.username)
		}
	}
	return locked, nil
}

// loadLegacyPasswordCandidates returns the accounts the backfill run at batchAt
// created whose password was never replaced.
//
// The batch is found by its clock. Every release that shipped the password ran
// the same backfill.go (one blob from v0.4.13 through v0.5.2), which inserted
// the accounts and the end_user_backfill_state row in a single transaction:
// created_at and password_changed_at from the column default now(), done_at
// from an explicit now(). On Postgres, the only engine those releases ran it
// against, now() is the transaction's start time, and the compatibility driver
// neither rewrites it nor splits the transaction, so each account of the batch
// has created_at equal to done_at to the microsecond (production: 44 accounts,
// all equal). Nothing rewrites either column afterwards.
//
// Within the batch, an unreplaced password means must_change_password still
// false and password_changed_at still at created_at: every path that writes a
// new hash moves password_changed_at. Both windows are judged here rather than
// in SQL because no interval arithmetic reads the same on Postgres and on the
// SQLite test fixtures.
func loadLegacyPasswordCandidates(ctx context.Context, tx *sql.Tx, batchAt time.Time) ([]legacyPasswordCandidate, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, username, password_hash, created_at, password_changed_at
		FROM end_users
		WHERE must_change_password = false
		ORDER BY username_normalized
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]legacyPasswordCandidate, 0)
	for rows.Next() {
		var candidate legacyPasswordCandidate
		var createdAt, changedAt time.Time
		if err := rows.Scan(&candidate.id, &candidate.username, &candidate.passwordHash, &createdAt, &changedAt); err != nil {
			return nil, err
		}
		if createdAt.Sub(batchAt).Abs() > neverChangedSlack {
			continue
		}
		if changedAt.After(createdAt.Add(neverChangedSlack)) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// lockLegacyPasswordAccountTx moves one account onto the replacement hash and
// revokes its sessions, reporting whether it did. The write is conditional on
// the hash that was verified: the old blue-green slot keeps serving while this
// runs, and a password its owner changed or an admin reset after the candidates
// were read no longer matches and is left as they set it.
func lockLegacyPasswordAccountTx(ctx context.Context, tx *sql.Tx, candidate legacyPasswordCandidate, replacementHash string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE end_users SET password_hash = ?, must_change_password = true,
			updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE id = ? AND password_hash = ? AND must_change_password = false
	`, replacementHash, candidate.id, candidate.passwordHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	// must_change_password is the second line behind this: the portal refuses
	// key endpoints to a flagged account, so a sign-in that raced this revoke
	// still cannot read a key.
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_user_sessions SET revoked_at = CURRENT_TIMESTAMP, revoke_reason = 'legacy_password_lock'
		WHERE end_user_id = ? AND revoked_at IS NULL
	`, candidate.id); err != nil {
		return false, err
	}
	return true, nil
}
