package clusterauth

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func backupDirPrefix() string { return util.PreClusterBackupDirPrefix }

func strayDirPrefix() string { return util.ClusterStrayDirPrefix }

// localFile is a credential file found in the auth directory.
type localFile struct {
	id      string
	path    string
	doc     map[string]any
	content []byte
	stamped bool
	version int64
	modTime time.Time
}

// scanLocalFiles reads every credential file in the auth directory, skipping
// the directories set-aside files live in and anything that is not a JSON
// object (single-node mode ignores those as well).
func (s *Store) scanLocalFiles() []localFile {
	root := s.baseDir()
	var files []localFile
	_ = filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if full != root && util.IsReservedAuthSubdir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		raw, err := os.ReadFile(full)
		if err != nil || len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		doc := make(map[string]any)
		if err = json.Unmarshal(raw, &doc); err != nil || doc == nil {
			return nil
		}
		id, err := s.normalizeID(full)
		if err != nil {
			return nil
		}
		content, _ := splitDocument(doc)
		canonical, err := canonicalJSON(content)
		if err != nil {
			return nil
		}
		file := localFile{id: id, path: full, doc: doc, content: canonical}
		if _, ok := doc[coreauth.CredentialVersionMetadataKey]; ok {
			file.stamped, file.version = true, coreauth.MetadataCredentialVersion(doc)
		}
		if info, errInfo := entry.Info(); errInfo == nil {
			file.modTime = info.ModTime()
		}
		files = append(files, file)
		return nil
	})
	return files
}

// reconcileLocalFiles aligns the auth directory with this node's view of the
// table. Mirrors are rewritten when missing or outdated and stale mirrors of
// deleted credentials removed. A file holding data the table does not have
// (a pre-cluster credential, a file dropped in by hand, an edited mirror) is
// moved into a <prefix><timestamp> directory, never deleted: it may hold the
// only copy of a token. Unstamped files younger than grace are left alone, as
// an upload writes its file just before the store records it.
func (s *Store) reconcileLocalFiles(prefix string, grace time.Duration) {
	now := time.Now()
	var backupDir string
	moved := 0
	present := make(map[string]struct{})
	for _, file := range s.scanLocalFiles() {
		present[file.id] = struct{}{}
		s.mu.Lock()
		entry := s.known[file.id]
		var (
			live, deleted bool
			version       int64
			exact, same   bool
		)
		if entry != nil {
			live, deleted, version = !entry.deleted, entry.deleted, entry.version
			exact = bytes.Equal(entry.content, file.content)
			// An import stores the normalised document, so a pre-cluster file
			// that only differs by normalisation holds nothing new.
			same = exact || bytes.Equal(entry.content, normalizedContent(file.doc))
		}
		s.mu.Unlock()
		young := !file.stamped && grace > 0 && now.Sub(file.modTime) < grace
		switch {
		case live:
			if file.stamped && file.version == version && exact {
				continue
			}
			if young {
				continue
			}
			if !same && (!file.stamped || file.version >= version) {
				if s.moveAside(file.id, file.path, prefix, &backupDir) {
					moved++
				}
			}
			s.syncMirror(file.id)
		case deleted:
			if file.stamped && file.version <= version {
				if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
					log.WithError(err).Warnf("cluster auth: remove mirror of deleted credential %s", file.id)
				}
				continue
			}
			if !young && s.moveAside(file.id, file.path, prefix, &backupDir) {
				moved++
			}
		default:
			if !young && s.moveAside(file.id, file.path, prefix, &backupDir) {
				moved++
			}
		}
	}
	s.mu.Lock()
	missing := make([]string, 0)
	for id, entry := range s.known {
		if _, ok := present[id]; !ok && !entry.deleted {
			missing = append(missing, id)
		}
	}
	s.mu.Unlock()
	for _, id := range missing {
		s.syncMirror(id)
	}
	if moved > 0 {
		log.Warnf("cluster auth: set aside %d local auth file(s) that the credential database does not hold or holds differently, in %s; in cluster mode credentials are added through the management API or a login", moved, backupDir)
	}
}

// normalizedContent is the canonical credential part of doc after the
// normalisation the file store applies on load.
func normalizedContent(doc map[string]any) []byte {
	probe := make(map[string]any, len(doc))
	for key, value := range doc {
		probe[key] = value
	}
	normalized := sdkauth.NormalizeAuthMetadata(probe, sdkauth.InferAuthProvider(probe))
	content, _ := splitDocument(normalized)
	canonical, err := canonicalJSON(content)
	if err != nil {
		return nil
	}
	return canonical
}

// moveAside moves an auth file into the set-aside directory, creating it on
// first use, and keeps its relative path so tenants stay apart.
func (s *Store) moveAside(id, source, prefix string, backupDir *string) bool {
	if *backupDir == "" {
		*backupDir = filepath.Join(s.baseDir(), prefix+time.Now().Format("20060102-150405"))
	}
	target := filepath.Join(*backupDir, filepath.FromSlash(id))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		log.WithError(err).Warnf("cluster auth: set aside %s", id)
		return false
	}
	if _, err := os.Stat(target); err == nil {
		target += "." + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if err := os.Rename(source, target); err != nil {
		log.WithError(err).Warnf("cluster auth: set aside %s", id)
		return false
	}
	return true
}
