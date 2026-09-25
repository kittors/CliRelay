package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestResolveUsageSpoolDirPrefersPersistentLocations(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv(config.EnvUsageSpoolDir, "")
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")

	cfg := &config.Config{AuthDir: authDir}
	if got, want := resolveUsageSpoolDir(cfg), filepath.Join(root, usageSpoolDirName); got != want {
		t.Fatalf("default spool dir = %q, want %q", got, want)
	}

	// The Docker layout mounts <parent>/data as the persistent volume.
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := resolveUsageSpoolDir(cfg), filepath.Join(root, "data", usageSpoolDirName); got != want {
		t.Fatalf("spool dir with a data volume = %q, want %q", got, want)
	}

	t.Setenv("WRITABLE_PATH", filepath.Join(root, "writable"))
	if got, want := resolveUsageSpoolDir(cfg), filepath.Join(root, "writable", usageSpoolDirName); got != want {
		t.Fatalf("spool dir with WRITABLE_PATH = %q, want %q", got, want)
	}

	cfg.DBResilience.UsageSpoolDir = filepath.Join(root, "configured")
	if got, want := resolveUsageSpoolDir(cfg), filepath.Join(root, "configured"); got != want {
		t.Fatalf("configured spool dir = %q, want %q", got, want)
	}
	t.Setenv(config.EnvUsageSpoolDir, filepath.Join(root, "from-env"))
	if got, want := resolveUsageSpoolDir(cfg), filepath.Join(root, "from-env"); got != want {
		t.Fatalf("spool dir from %s = %q, want %q", config.EnvUsageSpoolDir, got, want)
	}
}
