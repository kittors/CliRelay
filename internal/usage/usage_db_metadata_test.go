package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestFailedInitializerRemovesBothMetadataHandles(t *testing.T) {
	CloseDB()
	t.Cleanup(CloseDB)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	readDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer readDB.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	func() {
		usageDBLifecycleMu.Lock()
		defer usageDBLifecycleMu.Unlock()
		usageWriteLease.Lock()
		defer usageWriteLease.Unlock()
		usageDBMu.Lock()
		defer usageDBMu.Unlock()
		err = initOpenedDBLocked(db, readDB, "fixture", "sqlite", config.RequestLogStorageConfig{}, time.FixedZone("old", -7*3600), true)
	}()
	if err == nil {
		t.Fatal("closed initializer unexpectedly succeeded")
	}
	for _, handle := range []*sql.DB{db, readDB} {
		if _, ok := usageDBMetadataByHandle.Load(handle); ok {
			t.Error("failed initializer retained metadata handle")
		}
	}
	if err := InitDB(filepath.Join(t.TempDir(), "new.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	if got := metadataForUsageDB(getDB()).loc; got != time.UTC {
		t.Fatalf("new location = %v", got)
	}
}
