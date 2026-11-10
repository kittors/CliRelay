package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// These run the maintenance worker itself in a synctest bubble, so its timers
// follow the bubble's clock and hours pass in milliseconds.

// maintenanceTestStart is where the bubble's clock is set before InitDB, so code
// that reads the date sees a present-day one rather than the bubble's 2000.
var maintenanceTestStart = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// Production's request log storage settings: bodies off, the 60-minute
// interval, 7 days of metadata and 3 of content.
var maintenanceTestStorage = config.RequestLogStorageConfig{
	RetentionDays:          7,
	ContentRetentionDays:   3,
	CleanupIntervalMinutes: 60,
}

// openMaintenanceTestDB starts the usage database, and with it the maintenance
// worker, inside the bubble. The startup pass runs before it returns.
func openMaintenanceTestDB(t *testing.T, storage config.RequestLogStorageConfig) *sql.DB {
	t.Helper()
	keepBodyStorageToggle(t)
	time.Sleep(time.Until(maintenanceTestStart))
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), storage, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	db := getDB()
	// Retention waits for the rollup backfill, which the server's startup
	// marks done after the old slot drains; production's has been done since
	// July. Nothing here schedules that, so mark it as production has it.
	if err := setProjectionMarker(db, usageRollupBackfillMarker, rollupMarkerDone); err != nil {
		t.Fatalf("mark rollup backfill done: %v", err)
	}
	// Every goroutine has to block before the bubble's clock moves, so this
	// returns once the startup pass is over and the worker waits for the next.
	time.Sleep(time.Second)
	return db
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func unstrippedContentRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	return countRows(t, db, `SELECT count(*) FROM request_log_content WHERE bodies_stripped = 0`)
}

// writeBodiesWhileStorageOn stores a request with its bodies, as while an
// operator had body storage on, and turns storage off again without the
// panel's purge: only a maintenance pass strips these rows.
func writeBodiesWhileStorageOn(t *testing.T, rows int) {
	t.Helper()
	SetRequestLogBodyStorageEnabled(true)
	for i := 0; i < rows; i++ {
		insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	}
	SetRequestLogBodyStorageEnabled(false)
}

// sleepUntil moves the bubble's clock to start+at. The checkpoints the tests
// use fall between the worker's own timers, so at each one the worker is
// waiting and its last pass is complete.
func sleepUntil(start time.Time, at time.Duration) {
	time.Sleep(time.Until(start.Add(at)))
}

// On 2026-11-10 each process ran request log maintenance once, at startup. The
// loop started a new full-interval timer after every wakeup, and the write path
// wakes it whenever it trims content to the size cap, which production did on
// nearly every write; the hourly pass never came, so retention and the body
// purge waited for the next deploy. Here a wakeup comes every 47 s, as from a
// busy write path, and the passes still come on the hour.
func TestRequestLogMaintenanceRunsHourlyWhileWritesWakeIt(t *testing.T) {
	synctest.Test(t, testRequestLogMaintenanceRunsHourlyWhileWritesWakeIt)
}

func testRequestLogMaintenanceRunsHourlyWhileWritesWakeIt(t *testing.T) {
	db := openMaintenanceTestDB(t, maintenanceTestStorage)
	start := time.Now().Add(-time.Second)
	wakeUntil := func(at time.Duration) {
		t.Helper()
		for {
			left := time.Until(start.Add(at))
			if left <= 0 {
				return
			}
			triggerRequestLogCompaction()
			time.Sleep(min(left, 47*time.Second))
		}
	}

	// A request log that falls out of the 7-day retention 90 minutes in.
	expiresAt := start.AddDate(0, 0, -maintenanceTestStorage.RetentionDays).Add(90 * time.Minute)
	if _, err := db.Exec(`INSERT INTO request_logs (timestamp, model) VALUES (?, 'expiring')`,
		expiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert expiring request log: %v", err)
	}
	expiring := func() int {
		return countRows(t, db, `SELECT count(*) FROM request_logs WHERE model = 'expiring'`)
	}

	wakeUntil(10 * time.Minute)
	writeBodiesWhileStorageOn(t, 1)

	wakeUntil(59 * time.Minute)
	if n := unstrippedContentRowCount(t, db); n != 1 {
		t.Fatalf("rows with bodies at 59 min = %d, want 1: a wakeup only compacts", n)
	}
	wakeUntil(61 * time.Minute)
	if n := unstrippedContentRowCount(t, db); n != 0 {
		t.Fatalf("rows with bodies at 61 min = %d, want 0: the pass due at 60 min did not run", n)
	}
	if n := expiring(); n != 1 {
		t.Fatalf("expiring request log at 61 min = %d, want 1: not past retention until 90 min", n)
	}

	wakeUntil(70 * time.Minute)
	writeBodiesWhileStorageOn(t, 1)
	wakeUntil(119 * time.Minute)
	if n := expiring(); n != 1 {
		t.Fatalf("expired request log at 119 min = %d, want 1 until the next pass", n)
	}
	wakeUntil(121 * time.Minute)
	if n := expiring(); n != 0 {
		t.Fatalf("expired request log at 121 min = %d, want 0: the pass due at 120 min did not run", n)
	}
	if n := unstrippedContentRowCount(t, db); n != 0 {
		t.Fatalf("rows with bodies at 121 min = %d, want 0", n)
	}
}

// A backlog of rows to strip, from body storage having been on, is worked off
// in slices of requestBodyPurgePassRows a minute apart, not in one long pass;
// once it is gone the worker is back to the hourly pass.
func TestRequestLogMaintenanceBoundsEachPurgePass(t *testing.T) {
	synctest.Test(t, testRequestLogMaintenanceBoundsEachPurgePass)
}

func testRequestLogMaintenanceBoundsEachPurgePass(t *testing.T) {
	previousLimit := requestBodyPurgePassRows
	requestBodyPurgePassRows = 10
	t.Cleanup(func() { requestBodyPurgePassRows = previousLimit })

	storageOn := maintenanceTestStorage
	storageOn.StoreContent = true
	db := openMaintenanceTestDB(t, storageOn)
	writeBodiesWhileStorageOn(t, 23)
	// Storage off with 23 rows to strip; the worker restarts so its first pass
	// is the one that meets them.
	stopRequestLogMaintenance()
	start := time.Now()
	startRequestLogMaintenance(db, "sqlite")
	checkAt := func(at time.Duration, want int, why string) {
		t.Helper()
		sleepUntil(start, at)
		if n := unstrippedContentRowCount(t, db); n != want {
			t.Fatalf("rows with bodies at %s = %d, want %d: %s", at, n, want, why)
		}
	}

	checkAt(30*time.Second, 13, "the pass at 0 reads its 10")
	checkAt(90*time.Second, 3, "a catch-up pass at 1 min reads 10 more")
	checkAt(150*time.Second, 0, "a catch-up pass at 2 min reads the last 3")
	// With the backlog gone, rows written now wait for the hourly pass.
	sleepUntil(start, 3*time.Minute)
	writeBodiesWhileStorageOn(t, 1)
	checkAt(61*time.Minute, 1, "no catch-up is due, so the next pass is an hour after the last")
	checkAt(63*time.Minute, 0, "the hourly pass due at 62 min")
}
