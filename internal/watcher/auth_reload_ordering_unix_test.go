//go:build linux || darwin

package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/sys/unix"
)

func TestAuthReloadCannotResurrectDeletedAccountFromOlderSnapshot(t *testing.T) {
	authDir := t.TempDir()
	target := filepath.Join(authDir, "a-target.json")
	gate := filepath.Join(authDir, "z-snapshot-gate.json")
	callbacks := make(chan struct{}, 8)
	w := &Watcher{
		authDir:        authDir,
		lastAuthHashes: make(map[string]string),
		authQueue:      make(chan AuthUpdate, 8),
		reloadCallback: func(*config.Config) { callbacks <- struct{}{} },
	}
	w.SetConfig(&config.Config{AuthDir: authDir})
	defer w.stopAuthReloadTimer()
	if err := os.WriteFile(target, []byte(`{"type":"demo","label":"initial"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.addOrUpdateClient(target)
	<-callbacks
	w.clientsMutex.RLock()
	_, initiallyPresent := w.currentAuths["a-target.json"]
	w.clientsMutex.RUnlock()
	if !initiallyPresent {
		t.Fatal("fixture did not register target through real snapshot")
	}

	// WalkDir sorts names: the actual FileSynthesizer reads a-target.json before
	// this FIFO. Keep its writer open to hold only that snapshot at ReadFile EOF.
	if err := unix.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *os.File, 1)
	openErr := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(gate, os.O_WRONLY, 0o600)
		if err != nil {
			openErr <- err
			return
		}
		opened <- writer
	}()
	if err := os.WriteFile(target, []byte(`{"type":"demo","label":"updated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: target, Op: fsnotify.Write})
	var writer *os.File
	select {
	case writer = <-opened:
	case err := <-openErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("timer never reached actual FileSynthesizer FIFO read")
	}
	defer writer.Close()

	// Replace the pathname, retaining the original FIFO inode for the old reader.
	// New snapshots read a regular file and therefore do not wait on the gate.
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte(`{"type":"demo","label":"gate"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	removed := make(chan struct{})
	go func() {
		w.handleEvent(fsnotify.Event{Name: target, Op: fsnotify.Remove})
		close(removed)
	}()
	removalFinished := false
	select {
	case <-removed:
		removalFinished = true
	case <-time.After(time.Second):
		// A correctly serialized implementation blocks this newer refresh until
		// the old snapshot completes. The deadline only releases that test gate.
	}
	if removalFinished {
		w.clientsMutex.RLock()
		_, present := w.currentAuths["a-target.json"]
		w.clientsMutex.RUnlock()
		w.dispatchMu.Lock()
		deleted := w.pendingUpdates["a-target.json"].Action
		w.dispatchMu.Unlock()
		t.Logf("while old snapshot is paused: target present=%v pending action=%v", present, deleted)
		if present || deleted != AuthUpdateActionDelete {
			t.Fatalf("fixture did not delete target first: present=%v action=%v", present, deleted)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("remove did not finish after FIFO EOF")
	}
	for range 2 {
		select {
		case <-callbacks:
		case <-time.After(5 * time.Second):
			t.Fatal("both real snapshots did not finish after FIFO EOF")
		}
	}
	w.clientsMutex.RLock()
	_, resurrected := w.currentAuths["a-target.json"]
	w.clientsMutex.RUnlock()
	w.dispatchMu.Lock()
	action := w.pendingUpdates["a-target.json"].Action
	w.dispatchMu.Unlock()
	t.Logf("after old snapshot resumed: target present=%v pending action=%v; file remains deleted", resurrected, action)
	if resurrected || action != AuthUpdateActionDelete {
		t.Fatalf("deleted auth resurrected by stale snapshot: present=%v action=%v, want absent/delete", resurrected, action)
	}
}
