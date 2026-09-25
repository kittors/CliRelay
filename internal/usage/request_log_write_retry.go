package usage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Retry policy for request log writes that fail because the database is
// unreachable (a restart, or a primary failover that takes 30-60 seconds).
// These are variables only so tests can shorten them.
var (
	// requestLogLiveRetryBudget is how long the usage worker keeps retrying one
	// record before handing it to the spool. A planned switchover finishes well
	// inside it, so those never touch the disk.
	requestLogLiveRetryBudget = 20 * time.Second
	// requestLogWriteAttemptTimeout bounds one attempt, every statement
	// included, so a primary that vanished without closing its sockets costs
	// seconds instead of however long TCP takes to give up.
	requestLogWriteAttemptTimeout = 10 * time.Second
	requestLogWriteMinAttemptTime = 2 * time.Second
	requestLogRetryInitialBackoff = 100 * time.Millisecond
	requestLogRetryMaxBackoff     = 5 * time.Second
	// requestLogCommitSettleWindow is how long a COMMIT whose reply was lost
	// stays suspect. With synchronous replication the old primary makes a
	// transaction visible locally before the standby confirms it; if Patroni
	// then fences that primary (the client sees 57P01 and "committed locally,
	// but might not have been replicated"), the promoted standby never had it.
	// A retry that still reaches the old primary finds our own key and would
	// call the record stored, so such a match is checked again once this
	// window, longer than a Patroni failover, has passed.
	requestLogCommitSettleWindow = 90 * time.Second

	// writeRequestLogAttemptFunc is the single-attempt writer; tests replace it
	// to inject database failures.
	writeRequestLogAttemptFunc = writeRequestLogAttempt
)

const requestLogContentionMaxAttempt = 8

// errUsageDBUnavailable reports that no runtime database handle is open.
var errUsageDBUnavailable = errors.New("usage: runtime database is not available")

// usageWriteTx is the statement surface of the request log write path.
// *sql.Tx satisfies it, so rebuild paths and tests keep passing a plain
// transaction; the live writer passes a boundTx instead.
type usageWriteTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Commit() error
	Rollback() error
}

// boundTx runs every statement of tx under ctx. database/sql gives the
// BeginTx context to BEGIN (and pgx uses it for COMMIT), but statements issued
// through the plain methods run under context.Background() and would wait on a
// dead primary with no deadline at all.
type boundTx struct {
	tx  *sql.Tx
	ctx context.Context
}

func (b boundTx) Exec(query string, args ...any) (sql.Result, error) {
	return b.tx.ExecContext(b.ctx, query, args...)
}

func (b boundTx) Query(query string, args ...any) (*sql.Rows, error) {
	return b.tx.QueryContext(b.ctx, query, args...)
}

func (b boundTx) QueryRow(query string, args ...any) *sql.Row {
	return b.tx.QueryRowContext(b.ctx, query, args...)
}

func (b boundTx) Commit() error   { return b.tx.Commit() }
func (b boundTx) Rollback() error { return b.tx.Rollback() }

// transientDBErrorMarkers catch connection failures that reach us as plain
// strings, for example after a driver wrapped them with fmt.Errorf("%v").
var transientDBErrorMarkers = []string{
	"sql: database is closed",
	"driver: bad connection",
	"conn closed",
	"connection refused",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"server closed the connection",
	"failed to connect",
	"i/o timeout",
	"no such host",
	"the database system is shutting down",
	"the database system is starting up",
	"the database system is in recovery mode",
	"cannot execute insert in a read-only transaction",
	"committed locally, but might not have been replicated",
}

// commitUncertainError marks a failure of COMMIT itself: the server may or may
// not have committed, so a later duplicate of the same key is only trusted
// after requestLogCommitSettleWindow.
type commitUncertainError struct{ err error }

func (e *commitUncertainError) Error() string { return "commit outcome unknown: " + e.err.Error() }
func (e *commitUncertainError) Unwrap() error { return e.err }

// isTransientDBError reports whether err means the database could not be
// reached or could not accept writes at that moment, as opposed to rejecting
// the data. Only these are worth retrying and spooling: a rejected row fails
// the same way on every replay.
func isTransientDBError(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{
		errUsageDBUnavailable, driver.ErrBadConn, sql.ErrConnDone, sql.ErrTxDone,
		io.EOF, io.ErrUnexpectedEOF, context.DeadlineExceeded, net.ErrClosed,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) {
		return isTransientSQLState(sqlState.SQLState())
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || pgconn.Timeout(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientDBErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isTransientSQLState(code string) bool {
	switch {
	case strings.HasPrefix(code, "08"): // connection_exception class
		return true
	case code == "57P01", code == "57P02", code == "57P03": // admin/crash shutdown, cannot connect now
		return true
	case code == "25006": // read_only_sql_transaction: the pool still points at a demoted primary
		return true
	case code == "53300": // too_many_connections: every node reconnecting at once after a failover
		return true
	}
	return false
}

type requestLogWriteOutcome int

const (
	// requestLogWriteCommitted: this attempt committed, or an earlier attempt of
	// the same record already had (see requestLogWriteResult.duplicate).
	requestLogWriteCommitted requestLogWriteOutcome = iota
	// requestLogWriteTransient: the database stayed unreachable; the record is
	// intact and belongs in the spool.
	requestLogWriteTransient
	// requestLogWritePermanent: the database rejected the record itself.
	requestLogWritePermanent
)

type requestLogWriteResult struct {
	// endUserID is the account the write resolved for the API key, reused by
	// the TPM hook so it needs no lookup of its own.
	endUserID string
	// duplicate is set when the idempotency key was already claimed.
	duplicate bool
	// commitUncertainAt is when an attempt last lost its COMMIT reply; zero
	// when none did. A duplicate close after it may be that very commit, still
	// exposed to a failover discarding it (see requestLogCommitSettleWindow).
	commitUncertainAt time.Time
}

// duplicateNeedsRecheck reports whether a duplicate must be verified again
// later instead of being trusted now.
func (r requestLogWriteResult) duplicateNeedsRecheck(now time.Time) bool {
	return r.duplicate && !r.commitUncertainAt.IsZero() && now.Before(r.commitUncertainAt.Add(requestLogCommitSettleWindow))
}

// writeRequestLogWithRetry writes entry, retrying lock contention the way the
// write path always has and connection failures with exponential backoff for
// up to budget. A zero budget still makes one attempt (plus contention
// retries); the replayer and shutdown use that.
func writeRequestLogWithRetry(entry RequestLogEntry, budget time.Duration) (requestLogWriteResult, requestLogWriteOutcome, error) {
	started := time.Now()
	backoff := requestLogRetryInitialBackoff
	contention := 0
	var commitUncertainAt time.Time
	for {
		timeout := requestLogWriteAttemptTimeout
		if budget > 0 {
			remaining := budget - time.Since(started)
			timeout = min(max(remaining, requestLogWriteMinAttemptTime), requestLogWriteAttemptTimeout)
		}
		result, err := writeRequestLogAttemptFunc(entry, timeout)
		var uncertain *commitUncertainError
		if errors.As(err, &uncertain) {
			commitUncertainAt = time.Now()
		}
		result.commitUncertainAt = commitUncertainAt
		switch {
		case err == nil:
			return result, requestLogWriteCommitted, nil
		case isRetryableUsageWriteErr(err):
			// Rollup UPSERT deadlocks under concurrent same-key traffic.
			contention++
			if contention >= requestLogContentionMaxAttempt {
				return result, requestLogWriteTransient, err
			}
			time.Sleep(time.Duration(contention*contention) * time.Millisecond)
		case isTransientDBError(err):
			if time.Since(started)+backoff > budget {
				return result, requestLogWriteTransient, err
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, requestLogRetryMaxBackoff)
		default:
			return result, requestLogWritePermanent, err
		}
	}
}

// requestLogWritePlan is an entry with everything the write needs resolved.
type requestLogWritePlan struct {
	entry        RequestLogEntry
	tenantID     string
	endUserID    string
	cost         float64
	storeContent bool
}

// requestLogKeyIdentity is the api_keys row the write attributes a log to.
type requestLogKeyIdentity struct {
	found     bool
	tenantID  string
	id        string
	name      string
	endUserID string
}

// lookupRequestLogKeyIdentity reads the key's tenant, id, name and owner in
// one query. Unlike GetAPIKey it surfaces connection failures: resolving to
// "no such key" while the database is down would file the log under the
// system tenant with no owner, and that attribution would then be committed
// by the retry or replay once the database is back.
func lookupRequestLogKeyIdentity(db *sql.DB, apiKey string, timeout time.Duration) (requestLogKeyIdentity, error) {
	var identity requestLogKeyIdentity
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return identity, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var tenantID, id, name, endUserID sql.NullString
	err := db.QueryRowContext(ctx, `SELECT tenant_id, id, name, end_user_id FROM api_keys WHERE key = ?`, key).
		Scan(&tenantID, &id, &name, &endUserID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return identity, nil
	case err != nil && isTransientDBError(err):
		return identity, err
	case err != nil:
		// Anything else (an unexpected schema, say) keeps the historical
		// behaviour of GetAPIKey: log under the defaults rather than drop.
		return identity, nil
	}
	identity.found = true
	identity.tenantID = strings.TrimSpace(tenantID.String)
	identity.id = strings.TrimSpace(id.String)
	identity.name = strings.TrimSpace(name.String)
	identity.endUserID = strings.TrimSpace(endUserID.String)
	return identity, nil
}

// planRequestLogWrite resolves tenant, key identity and cost. It runs before
// the write transaction opens: the embedded test store has a single
// connection, and reading through the pool while this goroutine holds that
// connection in a transaction would deadlock.
func planRequestLogWrite(db *sql.DB, entry RequestLogEntry, timeout time.Duration) (requestLogWritePlan, error) {
	identity, err := lookupRequestLogKeyIdentity(db, entry.APIKey, timeout)
	if err != nil {
		return requestLogWritePlan{}, fmt.Errorf("resolve request log key: %w", err)
	}
	tenantID := strings.TrimSpace(entry.TrustedTenantID)
	if tenantID == "" {
		tenantID = identity.tenantID
	}
	plan := requestLogWritePlan{entry: entry, tenantID: normalizeTenantID(tenantID)}
	if identity.found {
		if plan.entry.APIKeyID == "" {
			plan.entry.APIKeyID = identity.id
		}
		// Persist the key's current name; the request-time snapshot is only a
		// fallback for keys without one.
		if identity.name != "" {
			plan.entry.APIKeyName = identity.name
		}
		plan.endUserID = identity.endUserID
	}
	// Cost always follows Model, never the model the upstream echoed back.
	plan.cost = CalculateCostV2ForTenant(plan.tenantID, plan.entry.Model, plan.entry.Tokens)
	plan.storeContent = requestLogShouldStoreContent(plan.entry)
	return plan, nil
}

// requestLogShouldStoreContent mirrors the body-storage policy: failed
// requests always keep a compact error payload so the management UI error
// modal can show the upstream failure even when full body storage is disabled.
func requestLogShouldStoreContent(entry RequestLogEntry) bool {
	return entry.DetailContent != "" ||
		(RequestLogBodyStorageEnabled() && (entry.InputContent != "" || entry.OutputContent != "")) ||
		(entry.Failed && strings.TrimSpace(entry.OutputContent) != "")
}

// writeRequestLogAttempt makes one attempt. A claimed idempotency key means
// the record is already stored; the attempt then rolls back and reports a
// duplicate without touching any projection.
func writeRequestLogAttempt(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
	db := getDB()
	if db == nil {
		return requestLogWriteResult{}, errUsageDBUnavailable
	}
	plan, err := planRequestLogWrite(db, entry, timeout)
	if err != nil {
		return requestLogWriteResult{}, err
	}
	result := requestLogWriteResult{endUserID: plan.endUserID}

	// Shared projection lock before opening a DB tx so exclusive rebuilds never
	// leave writers holding connections while waiting on the mutex (pool deadlock).
	usageProjectionMu.RLock()
	defer usageProjectionMu.RUnlock()

	// The transaction belongs to the usage store, not to the HTTP request that
	// produced the record: a client disconnect must not abort a write that has
	// already been decided. The deadline starts after the projection lock so a
	// local rebuild is never mistaken for a database outage.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin insert tx: %w", err)
	}
	tx := boundTx{tx: sqlTx, ctx: ctx}
	claimed, err := claimRequestLogIdempotencyKeyTx(tx, plan.entry.IdempotencyKey)
	if err != nil {
		_ = tx.Rollback()
		return result, fmt.Errorf("claim request log key: %w", err)
	}
	if !claimed {
		_ = tx.Rollback()
		result.duplicate = true
		return result, nil
	}
	if err := insertRequestLogRowTx(tx, plan); err != nil {
		_ = tx.Rollback()
		return result, err
	}
	if err := projectLogWriteTx(tx, plan.rollupEvent()); err != nil {
		_ = tx.Rollback()
		return result, err
	}
	if err := tx.Commit(); err != nil {
		if isTransientDBError(err) {
			// The COMMIT may have been applied before the connection died.
			return result, fmt.Errorf("commit log insert: %w", &commitUncertainError{err: err})
		}
		return result, fmt.Errorf("commit log insert: %w", err)
	}
	return result, nil
}

func (p requestLogWritePlan) rollupEvent() rollupEvent {
	return rollupEvent{
		TenantID:      p.tenantID,
		APIKeyID:      p.entry.APIKeyID,
		EndUserID:     p.endUserID,
		AuthSubjectID: p.entry.AuthSubjectID,
		Model:         p.entry.Model,
		Source:        p.entry.Source,
		ChannelName:   p.entry.ChannelName,
		Failed:        p.entry.Failed,
		Streaming:     p.entry.Streaming,
		LatencyMs:     p.entry.LatencyMs,
		FirstTokenMs:  p.entry.FirstTokenMs,
		Tokens:        p.entry.Tokens,
		Cost:          p.cost,
		At:            p.entry.Timestamp,
	}
}
