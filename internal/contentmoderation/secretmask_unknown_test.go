package contentmoderation

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func (s *secretStoreStub) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *secretStoreStub) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// gatedSettingsStore holds every read until gate is closed.
type gatedSettingsStore struct {
	inner *secretStoreStub
	gate  chan struct{}
}

func (s *gatedSettingsStore) Load(ctx context.Context, tenantID string) (SecretMaskingSettings, int64, error) {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return SecretMaskingSettings{}, 0, ctx.Err()
	}
	return s.inner.Load(ctx, tenantID)
}

func (s *gatedSettingsStore) Save(ctx context.Context, tenantID string, settings SecretMaskingSettings, expected int64) error {
	return s.inner.Save(ctx, tenantID, settings, expected)
}

func enabledIn(t *testing.T, store SecretMaskingStore, tenantID string) {
	t.Helper()
	writer := NewSecretMaskingRegistry()
	writer.SetStore(store)
	if _, err := writer.SetEnabled(context.Background(), tenantID, true); err != nil {
		t.Fatal(err)
	}
}

// A failed first read is kept for a while so a store that is down is not asked
// on every request, and while it is kept the settings stay unknown: every
// request is refused, not just the first. Only a read that succeeds lets the
// tenant's requests through, masked.
func TestGuardRequestStaysRefusedWhileSettingsStayUnknown(t *testing.T) {
	store := newSecretStoreStub()
	enabledIn(t, store, "tenant-a")
	store.setErr(errors.New("database is down"))
	registry := NewSecretMaskingRegistry() // a restart: nothing read yet
	registry.SetStore(store)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	registry.now = func() time.Time { return time.Unix(0, clock.Load()) }
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	body := chatRequest("deploy with " + testOpenAIKey)
	req, opts := guardOptions("tenant-a", body)
	refused := func(when string) {
		t.Helper()
		result := moderator.GuardRequest(context.Background(), req, opts)
		if !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
			t.Fatalf("%s: %+v", when, result)
		}
	}
	before := store.loadCount()
	for range 3 {
		refused("while the failed read is kept")
	}
	if reads := store.loadCount() - before; reads != 1 {
		t.Fatalf("the store was read %d times; a failed read is retried after a while, not on every request", reads)
	}
	clock.Add(int64(secretMaskingRetryAfterFailure + time.Second))
	refused("after the retry failed too")
	if reads := store.loadCount() - before; reads != 2 {
		t.Fatalf("the store was read %d times, want a retry", reads)
	}

	store.setErr(nil)
	clock.Add(int64(secretMaskingRetryAfterFailure + time.Second))
	result := moderator.GuardRequest(context.Background(), req, opts)
	if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatalf("once the store answered: %+v", result)
	}
}

// A first read that does not finish in time leaves the settings unknown too.
func TestGuardRequestRefusesWhileTheFirstReadHangs(t *testing.T) {
	inner := newSecretStoreStub()
	enabledIn(t, inner, "tenant-a")
	gated := &gatedSettingsStore{inner: inner, gate: make(chan struct{})}
	registry := NewSecretMaskingRegistry()
	registry.SetStore(gated)
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	req, opts := guardOptions("tenant-a", chatRequest("deploy with "+testOpenAIKey))
	if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("while the first read hangs: %+v", result)
	}
	close(gated.gate)
	deadline := time.Now().Add(10 * time.Second)
	for {
		result := moderator.GuardRequest(context.Background(), req, opts)
		if !result.Blocked {
			if result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
				t.Fatalf("once read: %+v", result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still refused after the store answered: %+v", result)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Settings read once are kept while the store fails, whatever they are: a
// tenant with masking on stays masked, and one with it off is served, so the
// settings store going down does not refuse the requests of every tenant. The
// cost: a tenant switched on by another node is served as off by this one
// until it can read the store again.
func TestGuardRequestKeepsTheLastReadSettingsWhileTheStoreFails(t *testing.T) {
	store := newSecretStoreStub()
	registry := NewSecretMaskingRegistry()
	registry.SetStore(store)
	if _, err := registry.SetEnabled(context.Background(), "tenant-on", true); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetEnabled(context.Background(), "tenant-off", false); err != nil {
		t.Fatal(err)
	}
	store.setErr(errors.New("database is down"))
	registry.InvalidateAll()
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	body := chatRequest("deploy with " + testOpenAIKey)
	req, opts := guardOptions("tenant-on", body)
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatalf("a tenant with masking on: %+v", result)
	}
	req, opts = guardOptions("tenant-off", body)
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload != nil {
		t.Fatalf("a tenant with masking off: %+v", result)
	}
}
