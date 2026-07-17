package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnforceLogDirSizeLimitDeletesOldest(t *testing.T) {
	dir := t.TempDir()

	writeLogFile(t, filepath.Join(dir, "old.log"), 60, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "mid.log"), 60, time.Unix(2, 0))
	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 60, time.Unix(3, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 120, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatalf("expected old.log to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log")); err != nil {
		t.Fatalf("expected mid.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
}

func TestEnforceLogDirSizeLimitSkipsProtected(t *testing.T) {
	dir := t.TempDir()

	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 200, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "other.log"), 50, time.Unix(2, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 100, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.log")); !os.IsNotExist(err) {
		t.Fatalf("expected other.log to be removed, stat error: %v", err)
	}
}

func TestEnforceLogDirSizeLimitCountsAndDeletesSpoolTemps(t *testing.T) {
	dir := t.TempDir()

	// Orphaned stream spool temps must count toward the cap and be deleted first.
	writeLogFile(t, filepath.Join(dir, "request-body-aaaa.tmp"), 80, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "response-body-bbbb.tmp"), 80, time.Unix(2, 0))
	writeLogFile(t, filepath.Join(dir, "request-abc.log"), 40, time.Unix(3, 0))
	writeLogFile(t, filepath.Join(dir, "unrelated.tmp"), 200, time.Unix(0, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 50, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("expected at least 2 spool temps deleted, got %d", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "request-body-aaaa.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected request-body temp removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "response-body-bbbb.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected response-body temp removed, stat error: %v", err)
	}
	// Unrelated *.tmp files are not managed by the log dir cleaner.
	if _, err := os.Stat(filepath.Join(dir, "unrelated.tmp")); err != nil {
		t.Fatalf("expected unrelated.tmp to remain, stat error: %v", err)
	}
}

func TestIsLogDirManagedFileName(t *testing.T) {
	cases := map[string]bool{
		"main.log":              true,
		"main.log.gz":           true,
		"v1-responses-x.log":    true,
		"request-body-123.tmp":  true,
		"response-body-456.tmp": true,
		"REQUEST-BODY-789.TMP":  true,
		"unrelated.tmp":         false,
		"notes.txt":             false,
		"":                      false,
	}
	for name, want := range cases {
		if got := isLogDirManagedFileName(name); got != want {
			t.Fatalf("isLogDirManagedFileName(%q)=%v want %v", name, got, want)
		}
	}
}

func writeLogFile(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()

	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set times: %v", err)
	}
}
