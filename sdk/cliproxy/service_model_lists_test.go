package cliproxy

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestModelListSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "upstream-model-lists.json")
	fetchedAt := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	models := []*ModelInfo{
		{
			ID:                  "gemini-3.1-pro-high",
			Object:              "model",
			OwnedBy:             "antigravity",
			Type:                "antigravity",
			DisplayName:         "Gemini 3.1 Pro (High)",
			ContextLength:       1048576,
			MaxCompletionTokens: 65535,
			Thinking:            &ModelThinkingSupport{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"low", "high"}},
		},
		{ID: "kimi-k9-preview", UpstreamModelID: "k9-preview", SupportedParameters: []string{"tools"}, UserDefined: true},
	}

	saved := &credentialModelLists{path: path}
	if !saved.store("ag-round-trip", "antigravity", models, fetchedAt) {
		t.Fatal("storing a first list reported no change")
	}
	if err := saved.persist(nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat saved lists: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("saved lists mode = %o, want 600", perm)
		}
	}

	restored := &credentialModelLists{path: path}
	if loaded := restored.load(); loaded != 1 {
		t.Fatalf("loaded = %d, want 1", loaded)
	}
	got, source := restored.lookup("ag-round-trip", "antigravity")
	if source != modelListSourceOwn {
		t.Fatalf("source = %q, want %q", source, modelListSourceOwn)
	}
	if !reflect.DeepEqual(got, models) {
		t.Fatalf("restored models differ:\n got %+v\nwant %+v", got, models)
	}
	if at := restored.lists["ag-round-trip"].fetchedAt; !at.Equal(fetchedAt) {
		t.Fatalf("restored fetchedAt = %s, want %s", at, fetchedAt)
	}
	if restored.store("ag-round-trip", "antigravity", models, fetchedAt.Add(time.Hour)) {
		t.Fatal("the same list reported as a change after a restart")
	}
}

func TestLookupBorrowsTheNewestListOfTheSameProvider(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	lists := &credentialModelLists{}
	lists.store("ag-older", "antigravity", listedModels("antigravity", "older-model"), at)
	lists.store("ag-newer", "antigravity", listedModels("antigravity", "newer-model"), at.Add(time.Hour))
	lists.store("xai-newest", "xai", listedModels("xai", "grok-model"), at.Add(2*time.Hour))

	if models, source := lists.lookup("ag-older", "antigravity"); source != modelListSourceOwn || !hasModelID(models, "older-model") {
		t.Fatalf("own list = %v (%s)", modelIDs(models), source)
	}
	if models, source := lists.lookup("ag-unlisted", "antigravity"); source != modelListSourceProvider || !hasModelID(models, "newer-model") {
		t.Fatalf("borrowed list = %v (%s), want the provider's newest", modelIDs(models), source)
	}
	if models, source := lists.lookup("kimi-unlisted", "kimi"); len(models) != 0 || source != "" {
		t.Fatalf("borrowed across providers: %v (%s)", modelIDs(models), source)
	}

	// What lookup hands out is a copy: registration must not be able to edit the
	// stored list.
	models, _ := lists.lookup("ag-older", "antigravity")
	models[0].ID = "edited"
	if again, _ := lists.lookup("ag-older", "antigravity"); !hasModelID(again, "older-model") {
		t.Fatalf("editing a looked-up list changed the stored one: %v", modelIDs(again))
	}
}

func TestStoreReportsOnlyRealChanges(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	lists := &credentialModelLists{}
	first := listedModels("antigravity", "a", "b")
	first[0].Created = at.Unix()
	if !lists.store("ag", "antigravity", first, at) {
		t.Fatal("first list not reported as a change")
	}

	// Antigravity stamps Created with the time of each listing.
	restamped := listedModels("antigravity", "b", "a")
	restamped[1].Created = at.Add(time.Hour).Unix()
	if lists.store("ag", "antigravity", restamped, at.Add(time.Hour)) {
		t.Fatal("a re-listing that only moved Created and order was reported as a change")
	}

	widened := listedModels("antigravity", "a", "b")
	widened[0].ContextLength = 2097152
	if !lists.store("ag", "antigravity", widened, at.Add(2*time.Hour)) {
		t.Fatal("a changed context length was not reported")
	}
	if lists.store("ag", "antigravity", nil, at.Add(3*time.Hour)) {
		t.Fatal("an empty answer was stored")
	}
	if models, _ := lists.lookup("ag", "antigravity"); len(models) != 2 {
		t.Fatalf("an empty answer replaced the list: %v", modelIDs(models))
	}
}
