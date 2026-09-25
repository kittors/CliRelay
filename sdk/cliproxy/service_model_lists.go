package cliproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
	log "github.com/sirupsen/logrus"
)

// Last known model lists of self-listing credentials.
//
// An Antigravity, xAI or Kimi credential registers the models its own upstream
// last listed for it. Those lists used to live only in the executors'
// process-wide caches, so every start began without them: registration had to ask
// each upstream again before the listener came up, and a credential whose
// upstream did not answer registered nothing at all. They are now kept per
// credential and written to a file in the state directory, and a start registers
// from what the previous process last saw.
//
// A list is only ever replaced by a newer live answer. An upstream that fails to
// answer leaves it as it is.

const modelListSnapshotVersion = 1

// Where a self-listing credential's registered models came from, as named by the
// branch of its registration log line.
const (
	modelListSourceOwn      = "last-known" // its own last live list
	modelListSourceProvider = "provider"   // the newest list another credential of its provider fetched
	modelListSourceFloor    = "floor"      // the compiled-in floor; nothing was ever fetched
)

type credentialModelList struct {
	provider  string
	models    []*ModelInfo
	fetchedAt time.Time
	signature string
}

// credentialModelLists holds the last live list of each self-listing credential.
type credentialModelLists struct {
	mu    sync.Mutex
	lists map[string]credentialModelList
	// path is the snapshot file; empty keeps the lists in memory only.
	path string
	// writeMu keeps snapshot writes in order, so an older one cannot land last.
	writeMu sync.Mutex
}

type modelListSnapshot struct {
	Version int                               `json:"version"`
	Lists   map[string]modelListSnapshotEntry `json:"lists"`
}

type modelListSnapshotEntry struct {
	Provider  string                   `json:"provider"`
	FetchedAt time.Time                `json:"fetched_at"`
	Models    []modelListSnapshotModel `json:"models"`
}

// modelListSnapshotModel carries UserDefined, which ModelInfo leaves out of JSON.
type modelListSnapshotModel struct {
	ModelInfo
	UserDefined bool `json:"user_defined,omitempty"`
}

func (c *credentialModelLists) setPath(path string) {
	c.mu.Lock()
	c.path = path
	c.mu.Unlock()
}

func (c *credentialModelLists) snapshotPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// lookup returns the list a credential registers: its own last list, else the
// newest list another credential of its provider fetched. With neither it
// returns nothing, and the caller falls back to the compiled-in floor.
//
// Borrowing another credential's list is what registration did before lists were
// kept per credential: a failed fetch answered with the executor's process-wide
// cache, which held whichever credential had answered last.
func (c *credentialModelLists) lookup(authID, provider string) ([]*ModelInfo, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if own, ok := c.lists[authID]; ok && own.provider == provider {
		return cloneModelInfos(own.models), modelListSourceOwn
	}
	var newest credentialModelList
	newestID := ""
	for id, list := range c.lists {
		if list.provider != provider {
			continue
		}
		if newestID == "" || list.fetchedAt.After(newest.fetchedAt) ||
			(list.fetchedAt.Equal(newest.fetchedAt) && id < newestID) {
			newest, newestID = list, id
		}
	}
	if newestID == "" {
		return nil, ""
	}
	return cloneModelInfos(newest.models), modelListSourceProvider
}

// store records a live answer and reports whether it differs from the list the
// credential had before.
func (c *credentialModelLists) store(authID, provider string, models []*ModelInfo, fetchedAt time.Time) bool {
	cloned := cloneModelInfos(models)
	if authID == "" || len(cloned) == 0 {
		return false
	}
	signature := modelListSignature(cloned)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil {
		c.lists = make(map[string]credentialModelList)
	}
	previous, had := c.lists[authID]
	c.lists[authID] = credentialModelList{provider: provider, models: cloned, fetchedAt: fetchedAt, signature: signature}
	return !had || previous.provider != provider || previous.signature != signature
}

// forget drops a removed credential's list.
func (c *credentialModelLists) forget(authID string) {
	c.mu.Lock()
	delete(c.lists, authID)
	c.mu.Unlock()
}

// load reads the lists the previous process saved and reports how many it kept.
// A missing or unreadable file leaves the lists empty, which is where a first
// start is anyway.
func (c *credentialModelLists) load() int {
	path := c.snapshotPath()
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("model discovery: cannot read the saved model lists at %s: %v", path, err)
		}
		return 0
	}
	var snapshot modelListSnapshot
	if errUnmarshal := json.Unmarshal(data, &snapshot); errUnmarshal != nil {
		log.Warnf("model discovery: ignoring unreadable saved model lists at %s: %v", path, errUnmarshal)
		return 0
	}
	if snapshot.Version != modelListSnapshotVersion {
		log.Warnf("model discovery: ignoring saved model lists at %s with version %d", path, snapshot.Version)
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil {
		c.lists = make(map[string]credentialModelList)
	}
	loaded := 0
	for id, entry := range snapshot.Lists {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if strings.TrimSpace(id) == "" || !serviceapp.IsSelfListingProvider(provider) {
			continue
		}
		if _, live := c.lists[id]; live {
			continue
		}
		models := make([]*ModelInfo, 0, len(entry.Models))
		for i := range entry.Models {
			model := entry.Models[i].ModelInfo
			if strings.TrimSpace(model.ID) == "" {
				continue
			}
			model.UserDefined = entry.Models[i].UserDefined
			models = append(models, &model)
		}
		if len(models) == 0 {
			continue
		}
		c.lists[id] = credentialModelList{
			provider:  provider,
			models:    models,
			fetchedAt: entry.FetchedAt,
			signature: modelListSignature(models),
		}
		loaded++
	}
	return loaded
}

// persist writes the lists of the credentials keep reports as still present.
func (c *credentialModelLists) persist(keep func(authID string) bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	path := c.path
	snapshot := modelListSnapshot{Version: modelListSnapshotVersion, Lists: make(map[string]modelListSnapshotEntry, len(c.lists))}
	for id, list := range c.lists {
		if keep != nil && !keep(id) {
			continue
		}
		entry := modelListSnapshotEntry{
			Provider:  list.provider,
			FetchedAt: list.fetchedAt.UTC(),
			Models:    make([]modelListSnapshotModel, 0, len(list.models)),
		}
		for _, model := range list.models {
			entry.Models = append(entry.Models, modelListSnapshotModel{ModelInfo: *model, UserDefined: model.UserDefined})
		}
		snapshot.Lists[id] = entry
	}
	c.mu.Unlock()

	if path == "" {
		return nil
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode model lists: %w", err)
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

// modelListSignature captures what registration derives from a list. Created is
// left out: some upstreams stamp it with the time of the listing.
func modelListSignature(models []*ModelInfo) string {
	parts := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		stable := modelListSnapshotModel{ModelInfo: *model, UserDefined: model.UserDefined}
		stable.Created = 0
		encoded, err := json.Marshal(stable)
		if err != nil {
			encoded = []byte(model.ID)
		}
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		clone := *model
		clone.SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
		clone.SupportedParameters = append([]string(nil), model.SupportedParameters...)
		if model.Thinking != nil {
			thinking := *model.Thinking
			thinking.Levels = append([]string(nil), model.Thinking.Levels...)
			clone.Thinking = &thinking
		}
		out = append(out, &clone)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
