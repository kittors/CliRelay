package usage

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	_ "modernc.org/sqlite"
)

// TestQueryLogsConcurrentWithInserts verifies that management read queries stay
// responsive and error-free while inserts run concurrently. This guards the
// read/write connection split: reads must not serialize behind (or be blocked by)
// writes on the single write connection.
func TestQueryLogsConcurrentWithInserts(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	// Seed a few rows so reads have data to scan.
	for i := 0; i < 5; i++ {
		InsertLog("sk-test", "tester", "glm-5.2", "source", "channel", "auth-1",
			false, time.Now().UTC(), 100, 20, TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			`{"model":"glm-5.2","messages":[]}`, "")
	}

	const writers = 4
	const writesPerWriter = 25
	const readers = 4
	const readsPerReader = 25

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	// Writers: insert rows continuously.
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				InsertLog("sk-test", "tester", "glm-5.2", "source", "channel", "auth-1",
					false, time.Now().UTC(), 50, 10, TokenStats{InputTokens: 8, OutputTokens: 4, TotalTokens: 12},
					`{"model":"glm-5.2","messages":[]}`, "")
			}
		}()
	}

	// Readers: query logs concurrently with the writers.
	readErrs := make(chan error, readers*readsPerReader)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerReader; i++ {
				result, err := QueryLogs(LogQueryParams{Page: 1, Size: 50, Days: 7})
				if err != nil {
					readErrs <- err
					continue
				}
				if result.Items == nil {
					readErrs <- errQueryNilItems
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for concurrent reads/writes to finish")
	}
	close(readErrs)
	for err := range readErrs {
		t.Fatalf("concurrent read returned error: %v", err)
	}

	// Sanity: the row count reflects at least the seeded + written rows.
	var total int64
	if err := db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&total); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	expected := int64(5 + writers*writesPerWriter)
	if total < expected {
		t.Fatalf("row count = %d, want at least %d", total, expected)
	}
}

// errQueryNilItems is a sentinel used by TestQueryLogsConcurrentWithInserts to
// report a nil items slice via the shared error channel.
var errQueryNilItems = newSentinelError("query logs returned nil items")

func newSentinelError(msg string) error { return &sentinelError{msg: msg} }

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

// TestQueryDistinctErrorIsLogged verifies that when a read helper hits a DB error,
// it logs a warning (not silently swallowed into the HTTP 500 body only). This
// guards the error-logging fix so future 500s leave a trace in main.log. It uses
// a closed handle passed directly to the helper, avoiding any global-state mutation.
func TestQueryDistinctErrorIsLogged(t *testing.T) {
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	// Open then immediately close a handle so any query against it fails.
	closedDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	if err := closedDB.Close(); err != nil {
		t.Fatalf("close memory db: %v", err)
	}

	_, err = queryDistinct(closedDB, "model", time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		t.Fatal("expected queryDistinct to error against a closed handle")
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel {
			continue
		}
		if entry.Message != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a warn-level log entry for the queryDistinct error; got %d entries", len(hook.AllEntries()))
	}
}
