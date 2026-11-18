package modeldiscovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

// Keeping the stored lists across restarts.
//
// The store used to live only in memory, so a restart emptied it. Until a live
// fetch succeeded again, registration had nothing but the compiled-in catalog,
// and every Codex model that only the manifest lists stopped routing. On
// 2026-10-11 a deploy restarted the service while the fetch could not reach the
// upstream, and Codex clients got "unknown provider" for every current model
// for as long as that lasted. Each live answer is now written to disk and
// restored at startup, before the first registration pass.

const snapshotFileVersion = 1

type snapshotFile struct {
	Version int                      `json:"version"`
	Lists   map[string]snapshotEntry `json:"lists"`
}

type snapshotEntry struct {
	TenantID  string                       `json:"tenant_id"`
	Provider  string                       `json:"provider"`
	FetchedAt time.Time                    `json:"fetched_at"`
	Models    []*sdkmodelcatalog.ModelInfo `json:"models"`
}

var (
	// snapshotPath is guarded by mu; an empty path keeps the store in memory.
	snapshotPath string
	// writeMu orders whole writes, so the file always holds the newest state.
	writeMu sync.Mutex
)

// SetSnapshotPath sets the file the stored lists are kept in across restarts. An
// empty path turns persistence off.
func SetSnapshotPath(path string) {
	mu.Lock()
	snapshotPath = strings.TrimSpace(path)
	mu.Unlock()
}

// LoadSnapshot restores the lists a previous process saved and reports how many
// it restored. A list already in the store came from a live answer of this
// process and is kept. The saved fetch time is kept too, so a restored list is
// re-fetched once it is older than FreshFor while still routing until then.
func LoadSnapshot() (int, error) {
	mu.Lock()
	path := snapshotPath
	mu.Unlock()
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var file snapshotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, fmt.Errorf("decode %s: %w", path, err)
	}
	if file.Version != snapshotFileVersion {
		return 0, fmt.Errorf("%s has version %d, want %d", path, file.Version, snapshotFileVersion)
	}

	restored := 0
	mu.Lock()
	defer mu.Unlock()
	for _, saved := range file.Lists {
		provider := NormalizeProvider(saved.Provider)
		if !IsShared(provider) {
			continue
		}
		models := cloneModels(saved.Models)
		if len(models) == 0 {
			continue
		}
		key := storeKey(saved.TenantID, provider)
		if _, exists := entries[key]; exists {
			continue
		}
		entries[key] = entry{models: models, signature: listSignature(models), fetchedAt: saved.FetchedAt}
		restored++
	}
	return restored, nil
}

// persist writes every stored list to the snapshot file.
func persist() error {
	writeMu.Lock()
	defer writeMu.Unlock()

	mu.Lock()
	path := snapshotPath
	if path == "" {
		mu.Unlock()
		return nil
	}
	file := snapshotFile{Version: snapshotFileVersion, Lists: make(map[string]snapshotEntry, len(entries))}
	for key, stored := range entries {
		tenantID, provider, _ := strings.Cut(key, "|")
		file.Lists[key] = snapshotEntry{
			TenantID:  tenantID,
			Provider:  provider,
			FetchedAt: stored.fetchedAt,
			Models:    cloneModels(stored.models),
		}
	}
	mu.Unlock()

	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode provider model lists: %w", err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic replaces path with data, so a reader never sees a torn file.
// Both slots of a blue-green deploy share the state directory and may write at
// the same time; each write is whole, and the later one wins.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return errWrite
	}
	if errClose := tmp.Close(); errClose != nil {
		return errClose
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		return errRename
	}
	tmpName = ""
	return nil
}
