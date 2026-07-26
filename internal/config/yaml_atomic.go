package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"
)

// configFileWriteMu serializes read-modify-write cycles against config.yaml.
//
// A process-wide mutex rather than a per-path one: a process owns exactly one
// config.yaml, and several independent subsystems rewrite it — management handlers,
// the DB-backed section cleanup in internal/usage, and the config watcher's reload
// chain. Those paths do not share a handler lock (the watcher chain holds none, and
// the management onConfigMutated hook runs after the handler releases h.mu), so the
// lock has to live below all of them.
//
// It must wrap the whole read-modify-write cycle, not just the write: two writers that
// each read the file first would otherwise still lose one side's changes.
var configFileWriteMu sync.Mutex

// WithConfigFileWriteLock runs fn while holding the config.yaml write lock. Every
// code path that rewrites config.yaml must go through this, otherwise it can
// interleave with the others and corrupt or clobber the file.
func WithConfigFileWriteLock(fn func() error) error {
	if fn == nil {
		return nil
	}
	configFileWriteMu.Lock()
	defer configFileWriteMu.Unlock()
	return fn()
}

// WriteYAMLFileAtomic replaces path with data via a temporary file and rename.
//
// This exists because in-place truncating writes (os.Create / O_TRUNC) corrupt the
// file when two of them overlap: both write from offset 0 into the same inode, the
// shorter write leaves the longer one's tail behind, and the result is a spliced file
// whose seam usually lands mid-line. A rename publishes the new contents in one step,
// so a reader or a competing writer never observes a partial file.
//
// data is re-parsed before anything is published: writing YAML that cannot be read
// back bricks the config for every later save, so it is worth rejecting up front.
func WriteYAMLFileAtomic(path string, data []byte) error {
	return writeYAMLFileAtomicWithRename(path, data, os.Rename)
}

// writeYAMLFileAtomicWithRename takes the rename function so tests can exercise the
// EBUSY/EXDEV fallback that bind mounts hit.
func writeYAMLFileAtomicWithRename(path string, data []byte, renameFile func(string, string) error) error {
	var probe yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("refusing to write unparseable YAML to %s: %w", path, err)
	}

	// Preserve the existing permissions; fall back to owner-only for a new file.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if renameFile == nil {
		renameFile = os.Rename
	}
	if err := renameFile(tmpPath, path); err != nil {
		// Bind mounts and cross-device layouts (Docker, some NAS setups) can reject
		// rename. Falling back to an in-place write keeps those deployments working;
		// the write lock is what protects it from interleaving.
		if isAtomicReplaceUnsupported(err) {
			if fallbackErr := writeFileInPlace(path, data, mode); fallbackErr != nil {
				return fmt.Errorf("atomic replace failed: %w; in-place write failed: %w", err, fallbackErr)
			}
			return nil
		}
		return err
	}
	return nil
}

func isAtomicReplaceUnsupported(err error) bool {
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV)
}

func writeFileInPlace(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
