package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Hosts are machines that run no node but whose state the console shows: the
// database host above all, whose free disk space SQL cannot see. Each runs
// cmd/clirelay-hostagent, which upserts the machine part of a node snapshot
// (package nodemetrics: CPU, memory, load, disks) into cluster_hosts.
//
// They live apart from cluster_nodes on purpose: a host is not a member. It
// takes no part in leadership, and nothing that counts the active nodes or
// probes them must see it.

// HostRoleDatabase is the role of the host that runs the database.
const HostRoleDatabase = "database"

// HostStatus is one row of cluster_hosts.
type HostStatus struct {
	HostID  string `json:"host_id"`
	Role    string `json:"role"`
	Version string `json:"version,omitempty"`
	// Snapshot is the host's latest snapshot (nodemetrics.Snapshot); only
	// its host and disks parts describe the machine.
	Snapshot   json.RawMessage `json:"snapshot"`
	SnapshotAt time.Time       `json:"snapshot_at"`
	// AgeSeconds is how old the snapshot is by the database clock, which
	// also stamped it, so a browser with a wrong clock still reads it right.
	AgeSeconds float64 `json:"age_seconds"`
}

const listHostsSQL = `
SELECT host_id, role, version, snapshot, snapshot_at,
       greatest(extract(epoch FROM now() - snapshot_at), 0)::float8
FROM cluster_hosts
ORDER BY role, host_id`

const upsertHostSQL = `
INSERT INTO cluster_hosts (host_id, role, version, snapshot, snapshot_at)
VALUES ($1, $2, $3, $4::jsonb, now())
ON CONFLICT (host_id) DO UPDATE SET
role        = EXCLUDED.role,
version     = EXCLUDED.version,
snapshot    = EXCLUDED.snapshot,
snapshot_at = EXCLUDED.snapshot_at`

// ListHosts reads every host's latest snapshot, oldest role first. A host
// that stopped reporting keeps its row; the console tells from AgeSeconds
// that it went quiet.
func ListHosts(ctx context.Context, db *sql.DB) ([]HostStatus, error) {
	if db == nil {
		return nil, errors.New("cluster: no database")
	}
	rows, err := db.QueryContext(ctx, listHostsSQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hosts []HostStatus
	for rows.Next() {
		var host HostStatus
		var snapshot []byte
		if err := rows.Scan(&host.HostID, &host.Role, &host.Version, &snapshot, &host.SnapshotAt, &host.AgeSeconds); err != nil {
			return nil, err
		}
		host.Snapshot = json.RawMessage(snapshot)
		host.SnapshotAt = host.SnapshotAt.UTC()
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// UpsertHost stores a host's latest snapshot under hostID.
func UpsertHost(ctx context.Context, db *sql.DB, hostID, role, version string, snapshot json.RawMessage) error {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return errors.New("cluster: host id is required")
	}
	if !json.Valid(snapshot) {
		return errors.New("cluster: host snapshot is not valid JSON")
	}
	_, err := db.ExecContext(ctx, upsertHostSQL, hostID, strings.TrimSpace(role), version, string(snapshot))
	return err
}
