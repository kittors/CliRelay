package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const allowlistTestProvider = "allowlist-test-provider"

func allowlistTestManager(t *testing.T, executor ProviderExecutor) *Manager {
	t.Helper()
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	if executor == nil {
		executor = &stubExecutor{id: allowlistTestProvider}
	}
	manager.RegisterExecutor(executor)
	for _, auth := range []*Auth{
		{ID: "account-a", Label: "Team Alpha", Provider: allowlistTestProvider, Status: StatusActive},
		{ID: "account-b", Label: "Team Alpha", Provider: allowlistTestProvider, Status: StatusActive},
		{ID: "account-c", Label: "Team Beta", Provider: allowlistTestProvider, Status: StatusActive},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func pickWithMetadata(manager *Manager, meta map[string]any) (*Auth, error) {
	auth, _, err := manager.pickNext(context.Background(), allowlistTestProvider, "", cliproxyexecutor.Options{Metadata: meta}, map[string]struct{}{})
	return auth, err
}

func TestAllowedAuthIDsFromMetadata(t *testing.T) {
	t.Parallel()

	if got := allowedAuthIDsFromMetadata(nil); got != nil {
		t.Fatalf("nil metadata = %v, want nil (no restriction)", got)
	}
	if got := allowedAuthIDsFromMetadata(map[string]any{"other": "x"}); got != nil {
		t.Fatalf("absent key = %v, want nil (no restriction)", got)
	}
	got := allowedAuthIDsFromMetadata(map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{" account-a ", "", "account-b", "account-a"}})
	if len(got) != 2 {
		t.Fatalf("parsed = %v, want account-a and account-b", got)
	}
	if _, ok := got["account-a"]; !ok {
		t.Fatalf("parsed = %v, want trimmed account-a", got)
	}
	got = allowedAuthIDsFromMetadata(map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []any{"account-c", 42}})
	if _, ok := got["account-c"]; !ok || len(got) != 1 {
		t.Fatalf("[]any parsed = %v, want only account-c", got)
	}
	// Present but empty or unreadable values must deny, never disable the check.
	for name, raw := range map[string]any{
		"empty slice": []string{},
		"nil value":   nil,
		"blank IDs":   []string{" ", ""},
		"wrong type":  "account-a",
		"number":      7,
	} {
		got := allowedAuthIDsFromMetadata(map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: raw})
		if got == nil || len(got) != 0 {
			t.Fatalf("%s: parsed = %v, want empty non-nil set", name, got)
		}
	}
}

func TestStrictAuthAllowlistRestrictsSelection(t *testing.T) {
	t.Parallel()
	manager := allowlistTestManager(t, nil)

	// Without an allowlist every credential stays eligible; FillFirst picks the first.
	if auth, err := pickWithMetadata(manager, nil); err != nil || auth == nil || auth.ID != "account-a" {
		t.Fatalf("no allowlist: auth=%v err=%v, want account-a", auth, err)
	}

	// Only listed credentials are eligible, even when the selector prefers another.
	meta := map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-c"}}
	for i := 0; i < 3; i++ {
		if auth, err := pickWithMetadata(manager, meta); err != nil || auth == nil || auth.ID != "account-c" {
			t.Fatalf("allowlist [account-c]: auth=%v err=%v", auth, err)
		}
	}

	// IDs that match no registered credential are simply never selected.
	meta = map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"missing", "account-b"}}
	if auth, err := pickWithMetadata(manager, meta); err != nil || auth == nil || auth.ID != "account-b" {
		t.Fatalf("allowlist [missing account-b]: auth=%v err=%v", auth, err)
	}
}

func TestStrictAuthAllowlistFailsClosed(t *testing.T) {
	t.Parallel()
	manager := allowlistTestManager(t, nil)

	for name, meta := range map[string]map[string]any{
		"empty allowlist":     {cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{}},
		"malformed allowlist": {cliproxyexecutor.AllowedAuthIDsMetadataKey: "account-a"},
		"only unknown IDs":    {cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"missing"}},
		"pinned outside list": {
			cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-c"},
			cliproxyexecutor.PinnedAuthMetadataKey:     "account-a",
		},
		"channel outside list": {
			cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-c"},
			"allowed-channels":                         "Team Alpha",
		},
	} {
		auth, err := pickWithMetadata(manager, meta)
		if err == nil || auth != nil {
			t.Fatalf("%s: auth=%v err=%v, want no credential", name, auth, err)
		}
		var selectionErr *Error
		if !errors.As(err, &selectionErr) || selectionErr.Code != "auth_not_found" {
			t.Fatalf("%s: err=%v, want auth_not_found", name, err)
		}
	}
}

func TestStrictAuthAllowlistNarrowsOtherScopes(t *testing.T) {
	t.Parallel()
	manager := allowlistTestManager(t, nil)

	meta := map[string]any{
		cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-b", "account-c"},
		"allowed-channels":                         "Team Alpha",
	}
	if auth, err := pickWithMetadata(manager, meta); err != nil || auth == nil || auth.ID != "account-b" {
		t.Fatalf("allowlist with channels: auth=%v err=%v, want account-b", auth, err)
	}

	meta = map[string]any{
		cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-b", "account-c"},
		cliproxyexecutor.PinnedAuthMetadataKey:     "account-c",
	}
	if auth, err := pickWithMetadata(manager, meta); err != nil || auth == nil || auth.ID != "account-c" {
		t.Fatalf("allowlist with pinned: auth=%v err=%v, want account-c", auth, err)
	}

	meta = map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-c"}}
	auth, _, _, err := manager.pickNextMixed(context.Background(), []string{allowlistTestProvider}, "", cliproxyexecutor.Options{Metadata: meta}, map[string]struct{}{})
	if err != nil || auth == nil || auth.ID != "account-c" {
		t.Fatalf("mixed pick: auth=%v err=%v, want account-c", auth, err)
	}
}

// recordingExecutor fails every call so the manager retries across every
// eligible credential, and records which credentials it was handed.
type recordingExecutor struct {
	stubExecutor
	mu   sync.Mutex
	seen []string
}

func (e *recordingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.seen = append(e.seen, auth.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, &Error{Code: "upstream_unavailable", Message: "fixture failure", Retryable: true, HTTPStatus: 503}
}

func (e *recordingExecutor) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

func TestStrictAuthAllowlistHoldsAcrossRetries(t *testing.T) {
	t.Parallel()
	executor := &recordingExecutor{stubExecutor: stubExecutor{id: allowlistTestProvider}}
	manager := allowlistTestManager(t, executor)

	meta := map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{"account-c"}}
	if _, err := manager.Execute(context.Background(), []string{allowlistTestProvider}, cliproxyexecutor.Request{Model: "fixture-model"}, cliproxyexecutor.Options{Metadata: meta}); err == nil {
		t.Fatal("Execute succeeded, want the fixture failure")
	}
	seen := executor.snapshot()
	if len(seen) == 0 {
		t.Fatal("executor was never called")
	}
	for _, id := range seen {
		if id != "account-c" {
			t.Fatalf("executor saw %v, want only account-c", seen)
		}
	}

	before := len(executor.snapshot())
	empty := map[string]any{cliproxyexecutor.AllowedAuthIDsMetadataKey: []string{}}
	if _, err := manager.Execute(context.Background(), []string{allowlistTestProvider}, cliproxyexecutor.Request{Model: "fixture-model"}, cliproxyexecutor.Options{Metadata: empty}); err == nil {
		t.Fatal("Execute with an empty allowlist succeeded")
	}
	if got := executor.snapshot(); len(got) != before {
		t.Fatalf("executor called with an empty allowlist: %v", got[before:])
	}
}
