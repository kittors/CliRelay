package configsync

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// FingerprintKey identifies one unit of configuration whose change calls for
// a reload: a domain, the tenant it belongs to, and a key inside the domain.
type FingerprintKey struct {
	Domain   string
	TenantID string
	Key      string
}

// Fingerprints maps every unit to a token that changes whenever the unit
// does: the row version where the table is versioned, otherwise the row count
// and newest update time.
type Fingerprints map[FingerprintKey]string

// Changed returns the units whose token differs from base, including units
// that appeared or disappeared, as events a Dispatcher can apply.
func (f Fingerprints) Changed(base Fingerprints) []cluster.ConfigEvent {
	var out []cluster.ConfigEvent
	for key, token := range f {
		if base[key] != token {
			out = append(out, cluster.ConfigEvent{Domain: key.Domain, TenantID: key.TenantID, Key: key.Key})
		}
	}
	for key := range base {
		if _, ok := f[key]; !ok {
			out = append(out, cluster.ConfigEvent{Domain: key.Domain, TenantID: key.TenantID, Key: key.Key})
		}
	}
	return out
}

// rowFingerprintKey is the key used for count/newest-update fingerprints,
// which do not name a single row.
const rowFingerprintKey = "rows"

// rowFingerprint describes a table without versions: its fingerprint is the
// row count and the newest update time, per tenant when tenantColumn is set.
type rowFingerprint struct {
	domain       string
	table        string
	tenantColumn string
	updated      string
}

// rowFingerprints cover the tables whose writers do not all bump a version.
// A count plus newest update catches inserts, deletes and updates; a delete
// and an insert with an older timestamp in the same interval would be missed,
// which a later change or restart repairs. These reloads are cheap and never
// rebuild executors, so a false positive only costs a few queries.
var rowFingerprints = []rowFingerprint{
	{domain: DomainAPIKeys, table: "api_keys", updated: "updated_at"},
	{domain: DomainPermissionProfiles, table: "api_key_permission_profiles", updated: "updated_at"},
	{domain: DomainEndUsers, table: "end_users", updated: "updated_at"},
	{domain: DomainModelConfigs, table: "model_configs", tenantColumn: "tenant_id", updated: "updated_at"},
	{domain: DomainModelConfigs, table: "auth_group_model_owner_mappings", tenantColumn: "tenant_id", updated: "updated_at"},
	{domain: DomainPricing, table: "model_pricing", updated: "updated_at"},
	{domain: DomainIPAccessRules, table: "ip_access_rules", updated: "updated_at"},
	{domain: DomainTenants, table: "tenants", updated: "updated_at"},
}

// SnapshotFingerprints reads the current fingerprint of every configuration
// unit. Tables a deployment does not have (SQLite test databases) are
// skipped; any other failure is returned, because a Resync that cannot tell
// what changed has to fall back to a full reload.
func SnapshotFingerprints(ctx context.Context, db *sql.DB) (Fingerprints, error) {
	if db == nil {
		return nil, fmt.Errorf("configsync: database not initialised")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(Fingerprints)
	if err := scanVersions(ctx, db, out, `SELECT tenant_id, setting_key, version FROM runtime_settings`, func(tenant, key string) FingerprintKey {
		return FingerprintKey{Domain: RuntimeSettingDomain(key), TenantID: NormalizeTenantID(tenant), Key: key}
	}); err != nil {
		return nil, err
	}
	if err := scanVersions(ctx, db, out, `SELECT tenant_id, '', version FROM routing_config`, func(tenant, _ string) FingerprintKey {
		return FingerprintKey{Domain: DomainRouting, TenantID: NormalizeTenantID(tenant)}
	}); err != nil {
		return nil, err
	}
	if err := scanVersions(ctx, db, out, `SELECT tenant_id, domain, version FROM config_versions`, func(tenant, domain string) FingerprintKey {
		return FingerprintKey{Domain: domain, TenantID: NormalizeTenantID(tenant)}
	}); err != nil {
		return nil, err
	}
	for _, spec := range rowFingerprints {
		if err := scanRows(ctx, db, out, spec); err != nil {
			// One unreadable table makes only its own domain undeterminable.
			// Those reloads are cheap, so mark it changed on every snapshot
			// rather than failing the whole comparison.
			log.WithError(err).Debug("configsync: row fingerprint unavailable")
			out[FingerprintKey{Domain: spec.domain, TenantID: systemTenantID, Key: rowFingerprintKey + ":" + spec.table}] = fmt.Sprintf("unreadable:%d", unreadableSeq.Add(1))
		}
	}
	return out, nil
}

// unreadableSeq makes the token of an unreadable table unique per snapshot.
var unreadableSeq atomic.Uint64

func scanVersions(ctx context.Context, db *sql.DB, out Fingerprints, query string, key func(tenant, name string) FingerprintKey) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTable(err) {
			return nil
		}
		return fmt.Errorf("configsync: fingerprint: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			tenant, name string
			version      int64
		)
		if err := rows.Scan(&tenant, &name, &version); err != nil {
			return fmt.Errorf("configsync: fingerprint: %w", err)
		}
		out[key(tenant, name)] = strconv.FormatInt(version, 10)
	}
	return rows.Err()
}

func scanRows(ctx context.Context, db *sql.DB, out Fingerprints, spec rowFingerprint) error {
	tenantExpr := "''"
	group := ""
	if spec.tenantColumn != "" {
		tenantExpr = spec.tenantColumn
		group = " GROUP BY " + spec.tenantColumn
	}
	query := fmt.Sprintf(`SELECT %s, COUNT(*), MAX(%s) FROM %s%s`, tenantExpr, spec.updated, spec.table, group)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTable(err) {
			return nil
		}
		return fmt.Errorf("configsync: fingerprint %s: %w", spec.table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			tenant any
			count  int64
			newest any
		)
		if err := rows.Scan(&tenant, &count, &newest); err != nil {
			return fmt.Errorf("configsync: fingerprint %s: %w", spec.table, err)
		}
		key := FingerprintKey{Domain: spec.domain, TenantID: NormalizeTenantID(fmt.Sprint(nilToEmpty(tenant))), Key: rowFingerprintKey + ":" + spec.table}
		out[key] = fmt.Sprintf("%d|%v", count, nilToEmpty(newest))
	}
	return rows.Err()
}

func nilToEmpty(v any) any {
	switch value := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(value)
	default:
		return value
	}
}

func isMissingTable(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || (strings.Contains(msg, "relation") && strings.Contains(msg, "does not exist"))
}

// localWrites records, per node, the versions of the writes that node made
// itself. The node applied those already; a Resync must not reload them
// again, least of all when the reload would rebuild executors.
var localWrites struct {
	sync.Mutex
	byNode map[string]Fingerprints
}

func noteLocalWrite(ev cluster.ConfigEvent) {
	if ev.Version <= 0 {
		return
	}
	node := cluster.Default().NodeID()
	key := FingerprintKey{Domain: ev.Domain, TenantID: NormalizeTenantID(ev.TenantID), Key: ev.Key}
	localWrites.Lock()
	defer localWrites.Unlock()
	if localWrites.byNode == nil {
		localWrites.byNode = make(map[string]Fingerprints)
	}
	if localWrites.byNode[node] == nil {
		localWrites.byNode[node] = make(Fingerprints)
	}
	localWrites.byNode[node][key] = strconv.FormatInt(ev.Version, 10)
}

// takeLocalWrites returns and clears the versions node wrote itself.
func takeLocalWrites(node string) Fingerprints {
	localWrites.Lock()
	defer localWrites.Unlock()
	notes := localWrites.byNode[node]
	delete(localWrites.byNode, node)
	return notes
}
