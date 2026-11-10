package usage

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// Production is PostgreSQL. There the primary key of request_log_content leads
// with tenant_id, which left the old walk no ordered path: each batch of 25
// re-read every detail row through the timestamp index. The flag and its
// partial index come from migrations, and the queries go through the compat
// driver.
func TestPostgresBodyPurgeReadsOnlyUnstrippedRows(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	keepBodyStorageToggle(t)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, bodyPurgeTestStorage, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	db := getDB()
	tenant := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, type, status, expires_at)
		VALUES (?, ?, ?, 'standard', 'active', now() + interval '1 day')`, tenant, "purge-"+tenant[:8], "purge-"+tenant[:8]); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	// Cleanups run last first: the rows go before their tenant.
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenants WHERE id = ?`, tenant) })
	truncatePostgresRuntimeTables(t, db)
	t.Cleanup(func() { truncatePostgresRuntimeTables(t, db) })

	var columnDefault string
	if err := db.QueryRow(`SELECT column_default FROM information_schema.columns
		WHERE table_name = 'request_log_content' AND column_name = 'bodies_stripped'`).Scan(&columnDefault); err != nil {
		t.Fatalf("read bodies_stripped column: %v", err)
	}
	if columnDefault != "0" {
		t.Fatalf("bodies_stripped default = %q, want 0 so rows older than the column are checked", columnDefault)
	}

	SetRequestLogBodyStorageEnabled(false)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	SetRequestLogBodyStorageEnabled(true)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, tenant, true, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, tenant, false, "")
	before := storedContentRows(t, db)
	if got := strippedFlags(before); !reflect.DeepEqual(got, []int{1, 0, 0, 0}) {
		t.Fatalf("flags = %v, want only the row written with storage off at 1", got)
	}

	SetRequestLogBodyStorageEnabled(false)
	result := purgeBodiesForTest(t)
	if result.CheckedRows != 3 || result.ClearedBodyRows != 3 || result.SanitizedDetailRows != 2 || result.DeletedContents != 1 {
		t.Fatalf("purge = %+v, want the 3 rows written with storage on stripped and the emptied one deleted", result)
	}
	after := storedContentRows(t, db)
	if len(after) != 3 {
		t.Fatalf("rows after the purge = %+v, want 3", after)
	}
	for _, row := range after {
		if row.BodiesStripped != 1 {
			t.Fatalf("log %d flag = %d after the purge, want 1", row.LogID, row.BodiesStripped)
		}
		if row.LogID == before[2].LogID {
			// The failed request keeps the upstream error for the error modal.
			if row.InputLen != 0 || row.OutputLen != before[2].OutputLen || strings.Contains(row.Detail, "secret-request") {
				t.Fatalf("failed request row = %+v", row)
			}
			continue
		}
		assertBodiesGone(t, row)
	}
	if result := purgeBodiesForTest(t); result.CheckedRows != 0 {
		t.Fatalf("second purge = %+v, want no rows read", result)
	}

	assertPostgresExplainUsesIndex(t, db, "unstripped log ids", "EXPLAIN "+unstrippedRequestLogIDsSQL,
		"idx_log_content_bodies_unstripped", 0)
	assertPostgresExplainUsesIndex(t, db, "unstripped rows", "EXPLAIN "+unstrippedContentRowsSQL(2),
		"idx_log_content_bodies_unstripped", 1, 2)
}
