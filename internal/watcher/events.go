// events.go implements fsnotify event handling for config and auth file changes.
// It normalizes paths, debounces noisy events, and triggers reload/update logic.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func matchProvider(provider string, targets []string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	for _, t := range targets {
		if strings.EqualFold(p, strings.TrimSpace(t)) {
			return p, true
		}
	}
	return p, false
}

func (w *Watcher) start(ctx context.Context) error {
	if errAddConfig := w.watcher.Add(w.configPath); errAddConfig != nil {
		log.Errorf("failed to watch config file %s: %v", w.configPath, errAddConfig)
		return errAddConfig
	}
	log.Debugf("watching config file: %s", w.configPath)

	if errAddAuthDir := w.addAuthDirectoryWatches(); errAddAuthDir != nil {
		log.Errorf("failed to watch auth directory %s: %v", w.authDir, errAddAuthDir)
		return errAddAuthDir
	}
	log.Debugf("watching auth directory: %s", w.authDir)

	go w.processEvents(ctx)

	w.reloadClients(true, nil, false)
	return nil
}

func (w *Watcher) processEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case errWatch, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Errorf("file watcher error: %v", errWatch)
		}
	}
}

// rewatchConfigFile re-arms the watch on the config path after an event that may have
// replaced its inode. Adding an already-watched path is a no-op, so this is safe to call
// on every config event.
//
// A rename-based replace publishes the new file before the old inode's watch is dropped,
// so the first attempt normally succeeds. The bounded retry covers delete-then-create
// sequences, where the path is briefly absent.
func (w *Watcher) rewatchConfigFile() {
	if w == nil || w.watcher == nil {
		return
	}
	if err := w.watcher.Add(w.configPath); err == nil {
		return
	} else if errors.Is(err, fsnotify.ErrClosed) {
		return
	}
	go func() {
		for attempt := 0; attempt < configRewatchAttempts; attempt++ {
			time.Sleep(configRewatchDelay)
			err := w.watcher.Add(w.configPath)
			if err == nil {
				return
			}
			if errors.Is(err, fsnotify.ErrClosed) {
				return
			}
		}
		// Worth a warning rather than a debug line: hot-reload is silently inactive from
		// here on, which is otherwise indistinguishable from "nothing changed".
		log.Warnf("config watch for %s could not be re-established; configuration hot-reload is inactive until restart", w.configPath)
	}()
}

func (w *Watcher) handleEvent(event fsnotify.Event) {
	if event.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			if errAdd := w.watcher.Add(event.Name); errAdd != nil {
				log.WithError(errAdd).Warnf("failed to watch tenant auth directory %s", event.Name)
			}
			return
		}
	}
	// Filter only relevant events: config file or auth-dir JSON files.
	//
	// Remove belongs here because config.yaml is replaced atomically (temp file +
	// rename). On inotify the watched inode sees CHMOD then REMOVE — never Write or
	// Create — so without Remove the replacement would be dropped entirely. Chmod is
	// watched too, but only to re-arm the watch: a bare permission change is not a
	// content change and must not trigger a reload.
	configOps := fsnotify.Write | fsnotify.Create | fsnotify.Rename | fsnotify.Remove
	configWatchOps := configOps | fsnotify.Chmod
	normalizedName := w.normalizeAuthPath(event.Name)
	normalizedConfigPath := w.normalizeAuthPath(w.configPath)
	normalizedAuthDir := w.normalizeAuthPath(w.authDir)
	isConfigEvent := normalizedName == normalizedConfigPath && event.Op&configWatchOps != 0
	authOps := fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename
	isAuthJSON := strings.HasPrefix(normalizedName, normalizedAuthDir) && strings.HasSuffix(normalizedName, ".json") && event.Op&authOps != 0
	if !isConfigEvent && !isAuthJSON {
		// Ignore unrelated files (e.g., cookie snapshots *.cookie) and other noise.
		return
	}

	now := time.Now()
	log.Debugf("file system event detected: %s %s", event.Op.String(), event.Name)

	// Handle config file changes
	if isConfigEvent {
		log.Debugf("config file change details - operation: %s, timestamp: %s", event.Op.String(), now.Format("2006-01-02 15:04:05.000"))
		// An atomic replace points the path at a new inode, and inotify watches inodes:
		// the old watch is dropped silently (IN_IGNORED, no error on the Errors channel),
		// so every later change would go unnoticed. Re-arm before reloading, otherwise the
		// first atomic save permanently kills config hot-reload on Linux.
		w.rewatchConfigFile()
		if event.Op&configOps != 0 {
			w.scheduleConfigReload()
		}
		return
	}

	// Handle auth directory changes incrementally (.json only)
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if w.shouldDebounceRemove(normalizedName, now) {
			log.Debugf("debouncing remove event for %s", filepath.Base(event.Name))
			return
		}
		// Atomic replace on some platforms may surface as Rename (or Remove) before the new file is ready.
		// Wait briefly; if the path exists again, treat as an update instead of removal.
		time.Sleep(replaceCheckDelay)
		if _, statErr := os.Stat(event.Name); statErr == nil {
			if unchanged, errSame := w.authFileUnchanged(event.Name); errSame == nil && unchanged {
				log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(event.Name))
				return
			}
			log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
			w.addOrUpdateClient(event.Name)
			return
		}
		if !w.isKnownAuthFile(event.Name) {
			log.Debugf("ignoring remove for unknown auth file: %s", filepath.Base(event.Name))
			return
		}
		log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
		w.removeClient(event.Name)
		return
	}
	if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		if unchanged, errSame := w.authFileUnchanged(event.Name); errSame == nil && unchanged {
			log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(event.Name))
			return
		}
		log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
		w.addOrUpdateClient(event.Name)
	}
}

func (w *Watcher) addAuthDirectoryWatches() error {
	return filepath.WalkDir(w.authDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return w.watcher.Add(path)
		}
		return nil
	})
}

func (w *Watcher) authFileUnchanged(path string) (bool, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return false, errRead
	}
	if len(data) == 0 {
		return false, nil
	}
	sum := sha256.Sum256(data)
	curHash := hex.EncodeToString(sum[:])

	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	prevHash, ok := w.lastAuthHashes[normalized]
	w.clientsMutex.RUnlock()
	if ok && prevHash == curHash {
		return true, nil
	}
	return false, nil
}

func (w *Watcher) isKnownAuthFile(path string) bool {
	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	_, ok := w.lastAuthHashes[normalized]
	return ok
}

func (w *Watcher) normalizeAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	if runtime.GOOS == "windows" {
		cleaned = strings.TrimPrefix(cleaned, `\\?\`)
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

func (w *Watcher) shouldDebounceRemove(normalizedPath string, now time.Time) bool {
	if normalizedPath == "" {
		return false
	}
	w.clientsMutex.Lock()
	if w.lastRemoveTimes == nil {
		w.lastRemoveTimes = make(map[string]time.Time)
	}
	if last, ok := w.lastRemoveTimes[normalizedPath]; ok {
		if now.Sub(last) < authRemoveDebounceWindow {
			w.clientsMutex.Unlock()
			return true
		}
	}
	w.lastRemoveTimes[normalizedPath] = now
	if len(w.lastRemoveTimes) > 128 {
		cutoff := now.Add(-2 * authRemoveDebounceWindow)
		for p, t := range w.lastRemoveTimes {
			if t.Before(cutoff) {
				delete(w.lastRemoveTimes, p)
			}
		}
	}
	w.clientsMutex.Unlock()
	return false
}
