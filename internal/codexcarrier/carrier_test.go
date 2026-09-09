package codexcarrier

import (
	"sync"
	"testing"
)

// reset clears package state so each case starts from a cold process.
func reset(t *testing.T) {
	t.Helper()
	Publish(nil)
	SetOverride("")
	t.Cleanup(func() {
		Publish(nil)
		SetOverride("")
	})
}

func TestResolveWithoutManifestUsesDefault(t *testing.T) {
	reset(t)

	if got := Resolve(); got != Default {
		t.Fatalf("Resolve() = %q, want %q on a cold manifest", got, Default)
	}
}

func TestResolvePrefersDefaultWhenManifestListsIt(t *testing.T) {
	reset(t)

	// Default is deliberately not first: manifests are ordered newest-first, and
	// picking the head would silently move image traffic onto the priciest model.
	Publish([]string{"gpt-9.9-future", "gpt-8.1", Default})
	if got := Resolve(); got != Default {
		t.Fatalf("Resolve() = %q, want %q", got, Default)
	}
}

func TestResolveFallsBackWhenDefaultIsRetired(t *testing.T) {
	reset(t)

	// The bug this package exists for: the carrier left the manifest and every
	// image request failed. Serving a costlier carrier beats serving nothing.
	Publish([]string{"gpt-9.9-future", "gpt-8.1"})
	if got := Resolve(); got != "gpt-9.9-future" {
		t.Fatalf("Resolve() = %q, want first surviving manifest entry", got)
	}
}

func TestResolveIgnoresExcludedManifestEntries(t *testing.T) {
	reset(t)

	Publish([]string{"gpt-5.3-codex-spark", "codex-auto-review", "gpt-8.1"})
	if got := Resolve(); got != "gpt-8.1" {
		t.Fatalf("Resolve() = %q, want the only eligible entry", got)
	}

	// A manifest of nothing but excluded entries must not resolve to one of them.
	Publish([]string{"gpt-5.3-codex-spark", "codex-auto-review"})
	if got := Resolve(); got != Default {
		t.Fatalf("Resolve() = %q, want %q when no entry is eligible", got, Default)
	}
}

func TestOverrideWinsOverManifest(t *testing.T) {
	reset(t)

	Publish([]string{Default, "gpt-8.1"})
	SetOverride("gpt-operator-choice")
	// Honoured even though the manifest omits it: manifests are per account and
	// per client version, so a list warmed elsewhere must not veto the operator.
	if got := Resolve(); got != "gpt-operator-choice" {
		t.Fatalf("Resolve() = %q, want the operator override", got)
	}

	SetOverride("   ")
	if got := Resolve(); got != Default {
		t.Fatalf("Resolve() = %q, want %q after the override is cleared", got, Default)
	}
}

func TestPublishNilKeepsResolutionUsable(t *testing.T) {
	reset(t)

	Publish([]string{"gpt-8.1"})
	Publish(nil)
	if got := Resolve(); got != Default {
		t.Fatalf("Resolve() = %q, want %q after the manifest is cleared", got, Default)
	}
}

func TestPublishSkipsBlankEntries(t *testing.T) {
	reset(t)

	Publish([]string{"", "   ", "gpt-8.1"})
	if got := Resolve(); got != "gpt-8.1" {
		t.Fatalf("Resolve() = %q, want blank entries skipped", got)
	}
}

func TestIsExcluded(t *testing.T) {
	cases := map[string]bool{
		"gpt-5.3-codex-spark": true,
		"codex-auto-review":   true,
		"GPT-5.3-Codex-Spark": true,
		"  ":                  true,
		"":                    true,
		"gpt-5.5":             false,
		"gpt-6-astra":         false,
	}
	for modelID, want := range cases {
		if got := IsExcluded(modelID); got != want {
			t.Fatalf("IsExcluded(%q) = %v, want %v", modelID, got, want)
		}
	}
}

// TestConcurrentPublishAndResolve is a race-detector target: the manifest is
// refreshed on a background schedule while request paths resolve carriers.
func TestConcurrentPublishAndResolve(t *testing.T) {
	reset(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				Publish([]string{"gpt-8.1", Default})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := Resolve(); got == "" {
					t.Errorf("Resolve() returned an empty carrier")
					return
				}
			}
		}()
	}
	wg.Wait()
}
