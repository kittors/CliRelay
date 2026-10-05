package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompleteProviderRetainsCompletedSessions(t *testing.T) {
	store := NewStore(time.Minute)
	store.Register("completed-state", "codex")
	store.Register("stale-state", "codex")

	store.Complete("completed-state")
	superseded := store.CompleteProvider("codex")
	if superseded != 1 {
		t.Fatalf("superseded = %d, want 1", superseded)
	}

	session, ok := store.Get("completed-state")
	if !ok {
		t.Fatal("expected completed session to remain queryable")
	}
	if session.Provider != "codex" || session.Status != StatusCompleted {
		t.Fatalf("session = %#v, want completed codex session", session)
	}
	// The replaced pending login stays as a tombstone so a status poll can
	// still tell it apart from an expired one; it is no longer pending.
	stale, ok := store.Get("stale-state")
	if !ok {
		t.Fatal("expected the superseded session to remain queryable as a tombstone")
	}
	if stale.Status != StatusSuperseded {
		t.Fatalf("stale status = %q, want %q", stale.Status, StatusSuperseded)
	}
	if store.IsPending("stale-state", "codex") {
		t.Fatal("a superseded session must not accept callbacks")
	}
	if again := store.CompleteProvider("codex"); again != 0 {
		t.Fatalf("second CompleteProvider superseded %d sessions, want 0", again)
	}
}

func TestValidateStateRejectsPathLikeValues(t *testing.T) {
	for _, state := range []string{"../state", "path/state", `path\state`, "bad state"} {
		if err := ValidateState(state); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("ValidateState(%q) error = %v, want ErrInvalidState", state, err)
		}
	}
}

func TestWriteCallbackFileForPendingWritesPayload(t *testing.T) {
	store := NewStore(time.Minute)
	store.Register("session-1", "gemini")
	authDir := t.TempDir()

	path, err := store.WriteCallbackFileForPending(authDir, "gemini", "session-1", " code ", " error ")
	if err != nil {
		t.Fatalf("WriteCallbackFileForPending() error = %v", err)
	}
	if filepath.Base(path) != ".oauth-gemini-session-1.oauth" {
		t.Fatalf("path = %q, want gemini callback file", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var payload callbackFilePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload.Code != "code" || payload.State != "session-1" || payload.Error != "error" {
		t.Fatalf("payload = %#v, want trimmed callback payload", payload)
	}
}

func TestWriteCallbackFileForPendingRejectsCompletedSession(t *testing.T) {
	store := NewStore(time.Minute)
	store.Register("session-1", "codex")
	store.Complete("session-1")

	_, err := store.WriteCallbackFileForPending(t.TempDir(), "codex", "session-1", "code", "")
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("WriteCallbackFileForPending() error = %v, want ErrNotPending", err)
	}
}

func TestWaitCallbackFileReadsAndRemovesPayload(t *testing.T) {
	store := NewStore(time.Minute)
	store.Register("session-1", "codex")
	authDir := t.TempDir()
	if _, err := WriteCallbackFile(authDir, "codex", "session-1", "code", ""); err != nil {
		t.Fatalf("WriteCallbackFile() error = %v", err)
	}

	payload, err := store.WaitCallbackFile(authDir, "codex", "session-1", time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitCallbackFile() error = %v", err)
	}
	if payload["code"] != "code" || payload["state"] != "session-1" {
		t.Fatalf("payload = %#v, want callback code and state", payload)
	}
	if _, err := os.Stat(filepath.Join(authDir, ".oauth-codex-session-1.oauth")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("callback file stat error = %v, want not exist", err)
	}
}

func TestWaitCallbackFileReturnsNotPending(t *testing.T) {
	store := NewStore(time.Minute)

	_, err := store.WaitCallbackFile(t.TempDir(), "codex", "session-1", time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("WaitCallbackFile() error = %v, want ErrNotPending", err)
	}
}
