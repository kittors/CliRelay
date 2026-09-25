package clusterauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// retouchDelay is how long after creating a directory the first mirror file
// in it is written again; see retouchLater.
const retouchDelay = 1500 * time.Millisecond

// normalizeID turns a credential ID or a path inside the auth directory into
// the slash-separated relative ID the table is keyed by.
func (s *Store) normalizeID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("cluster auth store: empty credential ID")
	}
	if filepath.IsAbs(raw) {
		rel, ok := relativeTo(s.baseDir(), raw)
		if !ok {
			return "", fmt.Errorf("cluster auth store: %s is outside the auth directory", raw)
		}
		raw = rel
	}
	clean := path.Clean(filepath.ToSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("cluster auth store: invalid credential ID %q", raw)
	}
	return clean, nil
}

// relativeTo returns target relative to base, resolving symlinks in either
// when the plain comparison fails (macOS /var is /private/var).
func relativeTo(base, target string) (string, bool) {
	rel := func(b, t string) (string, bool) {
		out, err := filepath.Rel(b, t)
		if err != nil || out == "." || out == ".." || strings.HasPrefix(out, ".."+string(filepath.Separator)) {
			return "", false
		}
		return out, true
	}
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	if out, ok := rel(base, target); ok {
		return out, true
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false
	}
	resolvedTarget := target
	if dir, errDir := filepath.EvalSymlinks(filepath.Dir(target)); errDir == nil {
		resolvedTarget = filepath.Join(dir, filepath.Base(target))
	}
	return rel(resolvedBase, resolvedTarget)
}

func (s *Store) mirrorPath(id string) string {
	return filepath.Join(s.baseDir(), filepath.FromSlash(id))
}

// applyRow makes r this node's view of its credential and rewrites the
// mirror to match. It never moves a credential back to an older version.
func (s *Store) applyRow(r row) {
	s.mu.Lock()
	changed := s.recordRowLocked(r, false)
	s.mu.Unlock()
	if changed {
		s.syncMirror(r.ID)
	}
}

// recordRowLocked updates the view of r.ID and reports whether it changed, or
// its mirror may be missing. Caller holds s.mu.
func (s *Store) recordRowLocked(r row, force bool) bool {
	entry := s.known[r.ID]
	if entry != nil && !force {
		if entry.version > r.Version {
			return false
		}
		if entry.version == r.Version && entry.deleted == r.Deleted {
			// Same version: nothing new, but the mirror may still be missing.
			return true
		}
	}
	next := &knownEntry{version: r.Version, deleted: r.Deleted, authIndex: r.AuthIndex}
	if entry != nil {
		if next.authIndex == "" {
			next.authIndex = entry.authIndex
		}
		next.history = pushHistory(entry.history, entry)
	}
	if !r.Deleted {
		content, err := canonicalObject(r.Content)
		if err != nil {
			log.WithError(err).Warnf("cluster auth: ignoring unreadable credential %s version %d", r.ID, r.Version)
			return false
		}
		runtime, err := decodeEntries(r.Runtime)
		if err != nil {
			runtime = make(map[string][]byte)
		}
		// This node's unflushed observations stay on top of the row's.
		if patch := s.patches[r.ID]; patch != nil {
			patch.overlay(runtime)
		}
		next.content, next.runtime = content, runtime
	}
	s.known[r.ID] = next
	return true
}

// recordWrite notes a compare-and-set this node committed.
func (s *Store) recordWrite(id string, version int64, content []byte) {
	s.mu.Lock()
	entry := s.known[id]
	if entry == nil || entry.version >= version {
		s.mu.Unlock()
		return
	}
	next := *entry
	next.history = pushHistory(entry.history, entry)
	next.version, next.content, next.deleted = version, content, false
	s.known[id] = &next
	s.mu.Unlock()
	s.syncMirror(id)
}

// forget handles a row that vanished from the table (removed by hand). It is
// treated as deleted: not recreated by late writes, mirror removed.
func (s *Store) forget(id string) {
	s.mu.Lock()
	entry := s.known[id]
	version := int64(0)
	if entry != nil {
		version = entry.version
	}
	s.known[id] = &knownEntry{version: version, deleted: true}
	s.mu.Unlock()
	s.dropPending(id)
	s.syncMirror(id)
}

func pushHistory(history map[int64][32]byte, entry *knownEntry) map[int64][32]byte {
	next := make(map[int64][32]byte, len(history)+1)
	for version, digest := range history {
		next[version] = digest
	}
	if entry != nil && !entry.deleted && entry.content != nil {
		next[entry.version] = contentDigest(entry.content)
	}
	if len(next) > versionHistoryDepth {
		versions := make([]int64, 0, len(next))
		for version := range next {
			versions = append(versions, version)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
		for _, version := range versions[:len(versions)-versionHistoryDepth] {
			delete(next, version)
		}
	}
	return next
}

// syncMirror writes (or removes) the mirror file of id from the current view.
// Writes are serialised, and always use the newest view, so an older version
// can never replace a newer one on disk.
func (s *Store) syncMirror(id string) {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	s.mu.Lock()
	entry := s.known[id]
	var (
		data    []byte
		version int64
		deleted bool
		err     error
	)
	if entry != nil {
		version, deleted = entry.version, entry.deleted
		if !deleted {
			data, err = mirrorDocument(entry.content, entry.runtime, entry.version)
		}
	}
	s.mu.Unlock()
	if entry == nil {
		return
	}
	target := s.mirrorPath(id)
	if deleted {
		s.removeDeletedMirror(id, target, version)
		return
	}
	if err != nil {
		log.WithError(err).Warnf("cluster auth: encode mirror of %s", id)
		return
	}
	if errWrite := s.writeMirrorFile(target, data); errWrite != nil {
		log.WithError(errWrite).Warnf("cluster auth: write mirror of %s", id)
	}
}

// removeDeletedMirror removes the mirror of a deleted credential. A file
// that is not an older mirror copy (no version stamp, or a newer one) is
// someone else's data and is moved aside instead.
func (s *Store) removeDeletedMirror(id, target string, version int64) {
	raw, err := os.ReadFile(target)
	if err != nil {
		return
	}
	stamped, fileVersion := mirrorStamp(raw)
	if stamped && fileVersion <= version {
		if errRemove := os.Remove(target); errRemove != nil && !os.IsNotExist(errRemove) {
			log.WithError(errRemove).Warnf("cluster auth: remove mirror of deleted credential %s", id)
		}
		return
	}
	var backupDir string
	s.moveAside(id, target, strayDirPrefix(), &backupDir)
}

// writeMirrorFile atomically replaces target with data unless it already holds it.
func (s *Store) writeMirrorFile(target string, data []byte) error {
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	dir := filepath.Dir(target)
	_, statErr := os.Stat(dir)
	newDir := os.IsNotExist(statErr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(target, data); err != nil {
		return err
	}
	if newDir {
		s.retouchLater(target, data)
	}
	return nil
}

// retouchLater writes a file placed in a directory created a moment ago once
// more. The watcher only watches a new directory after seeing it created, so
// the first file written into it can go unnoticed until something else in
// the auth directory changes.
func (s *Store) retouchLater(target string, data []byte) {
	time.AfterFunc(retouchDelay, func() {
		s.mirrorMu.Lock()
		defer s.mirrorMu.Unlock()
		if current, err := os.ReadFile(target); err != nil || !bytes.Equal(current, data) {
			return
		}
		_ = writeFileAtomic(target, data)
	})
}

// writeFileAtomic writes data to a temporary file next to target and renames
// it into place, so readers see the old or the new file, never a torn one.
// The temporary name does not end in .json, which the watcher ignores.
func writeFileAtomic(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".clirelay-mirror-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}

// mirrorStamp reads the version stamp of an auth file.
func mirrorStamp(raw []byte) (bool, int64) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, 0
	}
	if _, ok := doc[coreauth.CredentialVersionMetadataKey]; !ok {
		return false, 0
	}
	return true, coreauth.MetadataCredentialVersion(doc)
}
