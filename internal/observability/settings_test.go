package observability

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type memoryPersister struct {
	stored  *Settings
	saveErr error
	loadErr error
	saves   int
}

func (p *memoryPersister) Load() (Settings, bool, error) {
	if p.loadErr != nil {
		return Settings{}, false, p.loadErr
	}
	if p.stored == nil {
		return Settings{}, false, nil
	}
	return *p.stored, true, nil
}

func (p *memoryPersister) Save(s Settings) error {
	if p.saveErr != nil {
		return p.saveErr
	}
	p.saves++
	copied := s
	p.stored = &copied
	return nil
}

func newPersistedExporter(t *testing.T) (*Exporter, *memoryPersister) {
	t.Helper()
	e := NewExporter()
	e.now = func() time.Time { return time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC) }
	store := &memoryPersister{}
	if err := e.SetPersister(store); err != nil {
		t.Fatalf("SetPersister: %v", err)
	}
	return e, store
}

func TestSetEnabledIssuesATokenOnceAndKeepsItWhenDisabled(t *testing.T) {
	e, store := newPersistedExporter(t)

	settings, token, err := e.SetEnabled(true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+43 {
		t.Fatalf("token %q: want %s + 43 base64url characters", token, tokenPrefix)
	}
	if !settings.Enabled || settings.TokenSHA256 != hashToken(token) || settings.TokenHint != token[len(token)-4:] {
		t.Fatalf("settings after enable = %+v", settings)
	}
	if store.stored == nil || store.stored.TokenSHA256 != settings.TokenSHA256 {
		t.Fatalf("stored = %+v, want the new hash", store.stored)
	}
	if strings.Contains(store.stored.TokenSHA256, token) || store.stored.TokenHint == token {
		t.Fatal("the store must never hold the token itself")
	}

	// Enabling again neither shows nor replaces the token.
	again, token2, err := e.SetEnabled(true)
	if err != nil || token2 != "" || again.TokenSHA256 != settings.TokenSHA256 {
		t.Fatalf("second enable: token=%q hash changed=%v err=%v", token2, again.TokenSHA256 != settings.TokenSHA256, err)
	}

	off, token3, err := e.SetEnabled(false)
	if err != nil || token3 != "" || off.Enabled || off.TokenSHA256 != settings.TokenSHA256 {
		t.Fatalf("disable: %+v token=%q err=%v (the token must survive)", off, token3, err)
	}
}

func TestRotateTokenReplacesTheOldOne(t *testing.T) {
	e, _ := newPersistedExporter(t)
	_, first, _ := e.SetEnabled(true)
	settings, second, err := e.RotateToken()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if second == "" || second == first {
		t.Fatalf("rotated token %q, first %q", second, first)
	}
	if tokenMatches(first, settings.TokenSHA256) {
		t.Fatal("the old token still matches after rotation")
	}
	if !tokenMatches(second, settings.TokenSHA256) {
		t.Fatal("the new token does not match")
	}
}

func TestWritesAreSavedBeforeTheyApply(t *testing.T) {
	e, store := newPersistedExporter(t)
	store.saveErr = errors.New("database is read-only")
	if _, _, err := e.SetEnabled(true); err == nil {
		t.Fatal("enable succeeded although the save failed")
	}
	if e.Settings().Enabled || e.Settings().HasToken() {
		t.Fatalf("a failed save took effect: %+v", e.Settings())
	}
}

func TestWritesWithoutStoreAreRefused(t *testing.T) {
	e := NewExporter()
	if _, _, err := e.SetEnabled(true); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("enable without store: err = %v, want ErrNotPersistent", err)
	}
	if _, _, err := e.RotateToken(); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("rotate without store: err = %v, want ErrNotPersistent", err)
	}
	if e.Persistent() || e.Settings().Enabled {
		t.Fatal("an exporter without store reports itself persistent or enabled")
	}
}

func TestReloadAdoptsTheStoredSettings(t *testing.T) {
	e, store := newPersistedExporter(t)
	_, _, _ = e.SetEnabled(true)

	// Another node switched the endpoint off.
	other := *store.stored
	other.Enabled = false
	store.stored = &other
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if e.Settings().Enabled {
		t.Fatal("reload kept the endpoint on")
	}

	// A deleted row means the defaults.
	store.stored = nil
	if err := e.Reload(); err != nil || e.Settings().HasToken() {
		t.Fatalf("reload of a missing row: %+v err=%v", e.Settings(), err)
	}

	// An unreadable row is reported and leaves the last good settings.
	_, _, _ = e.SetEnabled(true)
	store.loadErr = errors.New("payload is not JSON")
	if err := e.Reload(); err == nil {
		t.Fatal("reload hid an unreadable row")
	}
	if !e.Settings().Enabled {
		t.Fatal("an unreadable row replaced the settings in force")
	}
}

func TestTokenMatches(t *testing.T) {
	hash := hashToken("crm_secret")
	if !tokenMatches("crm_secret", hash) || !tokenMatches("  crm_secret ", hash) {
		t.Fatal("the right token was refused")
	}
	for _, presented := range []string{"", "crm_secreT", "crm_secret2", hash} {
		if tokenMatches(presented, hash) {
			t.Fatalf("%q matched", presented)
		}
	}
	if tokenMatches("anything", "") {
		t.Fatal("an empty stored hash matched")
	}
}
