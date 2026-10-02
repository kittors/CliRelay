package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirPrefersPersistentLocations(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")

	if got, want := StateDir(authDir), root; got != want {
		t.Fatalf("default state dir = %q, want the auth directory's parent %q", got, want)
	}

	// The Docker layout mounts <parent>/data as the persistent volume.
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := StateDir(authDir), filepath.Join(root, "data"); got != want {
		t.Fatalf("state dir with a data volume = %q, want %q", got, want)
	}

	t.Setenv("WRITABLE_PATH", filepath.Join(root, "writable"))
	if got, want := StateDir(authDir), filepath.Join(root, "writable"); got != want {
		t.Fatalf("state dir with WRITABLE_PATH = %q, want %q", got, want)
	}
}

func TestStateDirIsNeverTheAuthDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	authDir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(filepath.Join(authDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A "data" directory inside the auth directory is still inside what the
	// watcher reads as credentials.
	if got := StateDir(authDir); got == authDir || filepath.Dir(got) == authDir {
		t.Fatalf("state dir %q is inside the auth directory %q", got, authDir)
	}
}

func TestStateDirWithoutAuthDirIsTheWorkingDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	if got := StateDir(""); got != "" {
		t.Fatalf("state dir without an auth directory = %q, want the working directory", got)
	}
}
