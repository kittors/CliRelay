package apikey

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// forgottenKeys records what the service tells the features that keep something
// per key when keys are deleted.
type forgottenKeys struct {
	calls [][]string
	err   error
}

func (f *forgottenKeys) delete(ids []string) error {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	f.calls = append(f.calls, cp)
	return f.err
}

func (f *forgottenKeys) all() []string {
	var out []string
	for _, call := range f.calls {
		out = append(out, call...)
	}
	sort.Strings(out)
	return out
}

func keyIDOf(t *testing.T, key string) string {
	t.Helper()
	row := usage.GetAPIKey(key)
	if row == nil || row.ID == "" {
		t.Fatalf("no key %q", key)
	}
	return row.ID
}

func TestDeletingAKeyTellsTheFeaturesThatKeepSomethingForIt(t *testing.T) {
	t.Run("delete by entry", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceKeys([]string{"sk-one", "sk-two"}); err != nil {
			t.Fatal(err)
		}
		id := keyIDOf(t, "sk-one")
		if _, err := svc.DeleteEntry("sk-one", nil, nil, false); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(forgotten.all(), []string{id}) {
			t.Fatalf("forgotten = %v, want only the deleted key's id %q", forgotten.calls, id)
		}
	})
	t.Run("delete by secret", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceKeys([]string{"sk-one", "sk-two"}); err != nil {
			t.Fatal(err)
		}
		id := keyIDOf(t, "sk-two")
		if err := svc.DeleteKey(" sk-two "); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(forgotten.all(), []string{id}) {
			t.Fatalf("forgotten = %v, want %q", forgotten.calls, id)
		}
		// A key that is not there leaves nothing to forget.
		forgotten.calls = nil
		if err := svc.DeleteKey("sk-missing"); err != nil || len(forgotten.calls) != 0 {
			t.Fatalf("err = %v, forgotten = %v", err, forgotten.calls)
		}
	})
	t.Run("patch to nothing", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceKeys([]string{"sk-one"}); err != nil {
			t.Fatal(err)
		}
		id := keyIDOf(t, "sk-one")
		if err := svc.PatchKey("sk-one", ""); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(forgotten.all(), []string{id}) {
			t.Fatalf("forgotten = %v, want %q", forgotten.calls, id)
		}
	})
	t.Run("a key that is renamed is still the same key", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceKeys([]string{"sk-one"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.PatchKey("sk-one", "sk-renamed"); err != nil {
			t.Fatal(err)
		}
		if len(forgotten.calls) != 0 {
			t.Fatalf("a rename must keep what was configured for the key: %v", forgotten.calls)
		}
	})
	t.Run("full replacement removes the ones left out", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceKeys([]string{"sk-one", "sk-two", "sk-three"}); err != nil {
			t.Fatal(err)
		}
		two, three := keyIDOf(t, "sk-two"), keyIDOf(t, "sk-three")
		forgotten.calls = nil // the first replacement removed nothing
		if err := svc.ReplaceKeys([]string{"sk-one"}); err != nil {
			t.Fatal(err)
		}
		want := []string{two, three}
		sort.Strings(want)
		if !reflect.DeepEqual(forgotten.all(), want) {
			t.Fatalf("forgotten = %v, want %v", forgotten.calls, want)
		}
	})
	t.Run("entry replacement removes the ones left out", func(t *testing.T) {
		setupTestDB(t)
		forgotten := &forgottenKeys{}
		svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
		if err := svc.ReplaceEntries([]config.APIKeyEntry{{Key: "sk-one"}, {Key: "sk-two"}}); err != nil {
			t.Fatal(err)
		}
		one, two := keyIDOf(t, "sk-one"), keyIDOf(t, "sk-two")
		forgotten.calls = nil
		if err := svc.ReplaceEntries([]config.APIKeyEntry{{ID: one, Key: "sk-one"}}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(forgotten.all(), []string{two}) {
			t.Fatalf("forgotten = %v, want %q only: the key that was kept must keep its configuration", forgotten.calls, two)
		}
	})
}

func TestAFailureToForgetAKeyDoesNotUndoItsDeletion(t *testing.T) {
	setupTestDB(t)
	forgotten := &forgottenKeys{err: errors.New("the policy table is not there")}
	svc := NewService(nil, WithKeyPolicyDeleter(forgotten.delete))
	if err := svc.ReplaceKeys([]string{"sk-one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteEntry("sk-one", nil, nil, false); err != nil {
		t.Fatalf("the key is deleted, so the request succeeds: %v", err)
	}
	if usage.GetAPIKey("sk-one") != nil {
		t.Fatal("the key is still there")
	}
	if len(forgotten.calls) != 1 {
		t.Fatalf("forgotten = %v", forgotten.calls)
	}
}

// Without a deleter (every caller that only reads) nothing changes.
func TestDeletingAKeyWithoutAKeyPolicyDeleter(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)
	if err := svc.ReplaceKeys([]string{"sk-one", "sk-two"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteKey("sk-one"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReplaceKeys(nil); err != nil {
		t.Fatal(err)
	}
}
