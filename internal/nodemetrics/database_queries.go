package nodemetrics

import (
	"context"
	"database/sql"
	"strings"
)

// The statistics queries behind DatabaseProbe. They run one after another on
// a single pooled connection, so a round takes one connection from the pool
// however many parts it reads. Parts that need more rights than the role has
// are left out; a context error ends the round, which then reads as a
// timeout.

// serverSQL reads what every role may read. current_setting(..., true)
// returns NULL instead of failing for a setting the server does not have
// (max_slot_wal_keep_size before 13).
const serverSQL = `
SELECT current_setting('server_version_num')::int,
       current_setting('server_version'),
       pg_postmaster_start_time(),
       pg_is_in_recovery(),
       current_setting('max_connections')::int,
       current_setting('superuser_reserved_connections')::int,
       pg_database_size(current_database()),
       (SELECT max(age(datfrozenxid)) FROM pg_database)::bigint,
       pg_size_bytes(current_setting('max_wal_size')),
       pg_size_bytes(current_setting('max_slot_wal_keep_size', true))`

// activitySQL counts sessions and finds the oldest transaction, idle
// transaction and lock wait. It leaves out its own session where that would
// count the probe as activity. $1 is the long-transaction age in seconds.
const activitySQL = `
SELECT count(*) FILTER (WHERE backend_type = 'client backend'),
       count(*) FILTER (WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid()),
       count(*) FILTER (WHERE backend_type = 'client backend' AND state = 'idle'),
       count(*) FILTER (WHERE backend_type = 'client backend' AND state LIKE 'idle in transaction%'),
       count(*) FILTER (WHERE backend_type = 'walsender'),
       coalesce(extract(epoch FROM max(now() - xact_start) FILTER (WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()))::float8, 0),
       count(*) FILTER (WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND xact_start < now() - $1 * interval '1 second'),
       coalesce(extract(epoch FROM max(now() - state_change) FILTER (WHERE backend_type = 'client backend' AND state LIKE 'idle in transaction%'))::float8, 0),
       count(*) FILTER (WHERE wait_event_type = 'Lock'),
       coalesce(extract(epoch FROM max(now() - query_start) FILTER (WHERE wait_event_type = 'Lock'))::float8, 0)
FROM pg_stat_activity`

// walDirSQL needs superuser or pg_monitor.
const walDirSQL = `SELECT count(*)::int, coalesce(sum(size), 0)::bigint FROM pg_ls_waldir()`

// slotsSQL13 reads the slots on PostgreSQL 13 and later; slotsSQLOld before,
// without wal_status and safe_wal_size. pg_current_wal_lsn() fails on a
// standby, hence the CASE.
const (
	slotsSQL13 = `
SELECT slot_name::text, coalesce(slot_type, ''), active, coalesce(wal_status, ''),
       CASE WHEN pg_is_in_recovery() OR restart_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint END,
       safe_wal_size
FROM pg_replication_slots ORDER BY slot_name`
	slotsSQLOld = `
SELECT slot_name::text, coalesce(slot_type, ''), active, '',
       CASE WHEN pg_is_in_recovery() OR restart_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint END,
       NULL::bigint
FROM pg_replication_slots ORDER BY slot_name`
)

// replicationSQL12 reads the replication connections on PostgreSQL 12 and
// later; replicationSQLOld before, without reply_time. client_addr is not
// read: the snapshot is stored and shown.
const (
	replicationSQL12 = `
SELECT coalesce(application_name, ''), coalesce(state, ''), coalesce(sync_state, ''),
       CASE WHEN pg_is_in_recovery() OR sent_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), sent_lsn)::bigint END,
       CASE WHEN pg_is_in_recovery() OR flush_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), flush_lsn)::bigint END,
       extract(epoch FROM now() - reply_time)::float8
FROM pg_stat_replication ORDER BY application_name, pid`
	replicationSQLOld = `
SELECT coalesce(application_name, ''), coalesce(state, ''), coalesce(sync_state, ''),
       CASE WHEN pg_is_in_recovery() OR sent_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), sent_lsn)::bigint END,
       CASE WHEN pg_is_in_recovery() OR flush_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), flush_lsn)::bigint END,
       NULL::float8
FROM pg_stat_replication ORDER BY application_name, pid`
)

// countersSQL reads the runtime database's cumulative counters.
const countersSQL = `
SELECT xact_commit, xact_rollback, blks_hit, blks_read, deadlocks, temp_bytes, stats_reset
FROM pg_stat_database WHERE datname = current_database()`

// queryDatabase reads one sample. counters is nil when pg_stat_database
// could not be read; err is set only when the server could not be read at
// all or the context ended.
func queryDatabase(ctx context.Context, db *sql.DB) (*Database, *databaseCounters, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()

	out := &Database{}
	var (
		versionNum                  int
		version                     string
		startedAt                   sql.NullTime
		maxWAL, maxSlotKeep, xidAge sql.NullInt64
		maxConnections, reserved    int
		inRecovery                  bool
		sizeBytes                   int64
	)
	if err := conn.QueryRowContext(ctx, serverSQL).Scan(&versionNum, &version, &startedAt, &inRecovery,
		&maxConnections, &reserved, &sizeBytes, &xidAge, &maxWAL, &maxSlotKeep); err != nil {
		return nil, nil, err
	}
	out.Version = serverVersion(version)
	if startedAt.Valid {
		at := startedAt.Time.UTC()
		out.StartedAt = &at
	}
	out.InRecovery = inRecovery
	out.SizeBytes = sizeBytes
	out.XIDAge = xidAge.Int64
	out.WAL = &DatabaseWAL{MaxWALSizeBytes: nullInt64(maxWAL), MaxSlotKeepBytes: nullInt64(maxSlotKeep)}

	// stop is true when a part failed because the context ended: the round
	// is over. Any other error (usually a missing right) leaves the part out.
	stop := func(err error) bool { return err != nil && ctx.Err() != nil }

	connections, activity, err := queryActivity(ctx, conn)
	if stop(err) {
		return nil, nil, ctx.Err()
	}
	if err == nil {
		connections.Max, connections.Reserved = maxConnections, reserved
		out.Connections, out.Activity = connections, activity
	}

	var files int
	var walBytes int64
	if err := conn.QueryRowContext(ctx, walDirSQL).Scan(&files, &walBytes); err == nil {
		out.WAL.Files, out.WAL.Bytes = &files, &walBytes
	} else if stop(err) {
		return nil, nil, ctx.Err()
	}

	slotsQuery := slotsSQLOld
	if versionNum >= 130000 {
		slotsQuery = slotsSQL13
	}
	slots, err := querySlots(ctx, conn, slotsQuery)
	if stop(err) {
		return nil, nil, ctx.Err()
	}
	out.Slots = slots

	replicationQuery := replicationSQLOld
	if versionNum >= 120000 {
		replicationQuery = replicationSQL12
	}
	replicas, err := queryReplicas(ctx, conn, replicationQuery)
	if stop(err) {
		return nil, nil, ctx.Err()
	}
	out.Replication = replicas

	counters, err := queryCounters(ctx, conn)
	if stop(err) {
		return nil, nil, ctx.Err()
	}
	return out, counters, nil
}

func queryActivity(ctx context.Context, conn *sql.Conn) (*DatabaseConnections, *DatabaseActivity, error) {
	c := &DatabaseConnections{}
	a := &DatabaseActivity{}
	err := conn.QueryRowContext(ctx, activitySQL, longTransactionAge.Seconds()).Scan(
		&c.Client, &c.Active, &c.Idle, &c.IdleInTransaction, &c.WALSenders,
		&a.LongestTransactionSeconds, &a.LongTransactions, &a.LongestIdleInTransactionSeconds,
		&a.LockWaits, &a.LongestLockWaitSeconds)
	if err != nil {
		return nil, nil, err
	}
	a.LongestTransactionSeconds = round3(a.LongestTransactionSeconds)
	a.LongestIdleInTransactionSeconds = round3(a.LongestIdleInTransactionSeconds)
	a.LongestLockWaitSeconds = round3(a.LongestLockWaitSeconds)
	return c, a, nil
}

func querySlots(ctx context.Context, conn *sql.Conn, query string) ([]DatabaseSlot, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var slots []DatabaseSlot
	for rows.Next() {
		var slot DatabaseSlot
		var retained, safe sql.NullInt64
		if err := rows.Scan(&slot.Name, &slot.Type, &slot.Active, &slot.WALStatus, &retained, &safe); err != nil {
			return nil, err
		}
		slot.RetainedBytes, slot.SafeWALSizeBytes = nullInt64(retained), nullInt64(safe)
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

func queryReplicas(ctx context.Context, conn *sql.Conn, query string) ([]DatabaseReplica, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var replicas []DatabaseReplica
	for rows.Next() {
		var replica DatabaseReplica
		var sent, flush sql.NullInt64
		var reply sql.NullFloat64
		if err := rows.Scan(&replica.Name, &replica.State, &replica.SyncState, &sent, &flush, &reply); err != nil {
			return nil, err
		}
		replica.SentLagBytes, replica.FlushLagBytes = nullInt64(sent), nullInt64(flush)
		if reply.Valid {
			age := round3(max(reply.Float64, 0))
			replica.ReplyAgeSeconds = &age
		}
		replicas = append(replicas, replica)
	}
	return replicas, rows.Err()
}

func queryCounters(ctx context.Context, conn *sql.Conn) (*databaseCounters, error) {
	var c databaseCounters
	var commits, rollbacks, hit, read, deadlocks, tempBytes int64
	var reset sql.NullTime
	if err := conn.QueryRowContext(ctx, countersSQL).Scan(&commits, &rollbacks, &hit, &read, &deadlocks, &tempBytes, &reset); err != nil {
		return nil, err
	}
	c.commits, c.rollbacks = nonNegative(commits), nonNegative(rollbacks)
	c.hit, c.read = nonNegative(hit), nonNegative(read)
	c.deadlock, c.tempBytes = nonNegative(deadlocks), nonNegative(tempBytes)
	if reset.Valid {
		c.reset = reset.Time.UTC()
	}
	return &c, nil
}

// serverVersion keeps the version number of server_version: packaged builds
// append a suffix such as " (Debian 15.19-1.pgdg120+1)".
func serverVersion(v string) string {
	v = strings.TrimSpace(v)
	if head, _, found := strings.Cut(v, " "); found {
		return head
	}
	return v
}

func nullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
