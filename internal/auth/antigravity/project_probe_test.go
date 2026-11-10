package antigravity

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestShouldSkipProjectIDProbe(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	t.Run("empty metadata probes", func(t *testing.T) {
		if ShouldSkipProjectIDProbe(nil, now, 0) {
			t.Fatal("nil metadata should not skip")
		}
		if ShouldSkipProjectIDProbe(map[string]any{}, now, 0) {
			t.Fatal("empty metadata should not skip")
		}
	})

	t.Run("existing project id skips", func(t *testing.T) {
		meta := map[string]any{"project_id": "proj-1"}
		if !ShouldSkipProjectIDProbe(meta, now, 0) {
			t.Fatal("project_id should skip probe")
		}
	})

	t.Run("recent unavailable verdict skips", func(t *testing.T) {
		meta := map[string]any{
			MetaProjectIDUnavailable: true,
			MetaProjectIDProbedAt:    now.Add(-23 * time.Hour).UnixMilli(),
		}
		if !ShouldSkipProjectIDProbe(meta, now, 0) {
			t.Fatal("a verdict inside its window should skip probe")
		}
	})

	t.Run("expired unavailable verdict probes again", func(t *testing.T) {
		meta := map[string]any{
			MetaProjectIDUnavailable: true,
			MetaProjectIDProbedAt:    now.Add(-25 * time.Hour).UnixMilli(),
		}
		if ShouldSkipProjectIDProbe(meta, now, 0) {
			t.Fatal("a verdict older than ProjectIDUnavailableRetryAfter must not block discovery")
		}
	})

	t.Run("unavailable flag without timestamp probes", func(t *testing.T) {
		meta := map[string]any{MetaProjectIDUnavailable: true}
		if ShouldSkipProjectIDProbe(meta, now, 0) {
			t.Fatal("a verdict nobody can date must not block discovery forever")
		}
	})

	t.Run("recent probe backoff skips", func(t *testing.T) {
		meta := map[string]any{MetaProjectIDProbedAt: now.Add(-5 * time.Minute).UnixMilli()}
		if !ShouldSkipProjectIDProbe(meta, now, 30*time.Minute) {
			t.Fatal("recent probe should skip within backoff")
		}
	})

	t.Run("stale probe allows retry", func(t *testing.T) {
		meta := map[string]any{MetaProjectIDProbedAt: now.Add(-2 * time.Hour).UnixMilli()}
		if ShouldSkipProjectIDProbe(meta, now, 30*time.Minute) {
			t.Fatal("stale probe should allow retry")
		}
	})
}

// The markers production credentials carry: written in 2026-09 with the wrong
// discovery client and, under the old rule, never probed again (#1032). JSON
// round trips store the timestamp as a float64.
func TestShouldSkipProjectIDProbeRecoversSeptemberMarkers(t *testing.T) {
	meta := map[string]any{
		MetaProjectIDUnavailable: true,
		MetaProjectIDProbedAt:    float64(time.Date(2026, 9, 16, 9, 22, 0, 0, time.UTC).UnixMilli()),
		MetaProjectIDProbeErr:    "no project_id in response",
	}
	if ShouldSkipProjectIDProbe(meta, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0) {
		t.Fatal("a September verdict must not keep blocking discovery in November")
	}
}

func TestMarkProjectIDProbeFailureTerminal(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	meta := map[string]any{}
	MarkProjectIDProbeFailure(meta, ErrNoProjectID, now)
	if meta[MetaProjectIDUnavailable] != true {
		t.Fatalf("unavailable = %#v, want true", meta[MetaProjectIDUnavailable])
	}
	if meta[MetaProjectIDProbedAt] != now.UnixMilli() {
		t.Fatalf("probed_at = %#v, want %d", meta[MetaProjectIDProbedAt], now.UnixMilli())
	}
	if !ShouldSkipProjectIDProbe(meta, now.Add(time.Hour), 0) {
		t.Fatal("a terminal verdict should hold off the next probe")
	}
	if !ShouldSkipProjectIDProbe(meta, now.Add(ProjectIDUnavailableRetryAfter-time.Minute), 30*time.Minute) {
		t.Fatal("the short back-off must not shorten a terminal verdict")
	}
	if ShouldSkipProjectIDProbe(meta, now.Add(ProjectIDUnavailableRetryAfter+time.Minute), 0) {
		t.Fatal("a terminal verdict should expire after ProjectIDUnavailableRetryAfter")
	}
}

func TestMarkProjectIDProbeFailureTransient(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	meta := map[string]any{}
	MarkProjectIDProbeFailure(meta, errors.New("http 503: busy"), now)
	if meta[MetaProjectIDUnavailable] == true {
		t.Fatal("transient error must not set unavailable")
	}
	if !ShouldSkipProjectIDProbe(meta, now.Add(time.Minute), 30*time.Minute) {
		t.Fatal("transient failure should backoff")
	}
	if ShouldSkipProjectIDProbe(meta, now.Add(31*time.Minute), 30*time.Minute) {
		t.Fatal("transient failure should retry after backoff")
	}
}

func TestMarkProjectIDProbeFailureTransientDropsEarlierVerdict(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	meta := map[string]any{
		MetaProjectIDUnavailable: true,
		MetaProjectIDProbedAt:    now.Add(-48 * time.Hour).UnixMilli(),
	}
	MarkProjectIDProbeFailure(meta, fmt.Errorf("execute request: %w", errors.New("connection reset")), now)
	if _, ok := meta[MetaProjectIDUnavailable]; ok {
		t.Fatal("a failure to reach Google is not a verdict and must drop the old one")
	}
	if ShouldSkipProjectIDProbe(meta, now.Add(31*time.Minute), 30*time.Minute) {
		t.Fatal("after a transient failure only the short back-off applies")
	}
}

func TestIsTerminalProjectProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "no project", err: ErrNoProjectID, want: true},
		{name: "no project wrapped", err: fmt.Errorf("%w (ineligible tiers: INELIGIBLE_ACCOUNT)", ErrNoProjectID), want: true},
		{name: "no project flattened", err: errors.New("probe: no project_id in response"), want: true},
		{name: "client refused", err: fmt.Errorf("%w: upgrade", ErrUnsupportedClient), want: true},
		{name: "onboarding still running", err: fmt.Errorf("%w after 5 polls", ErrOnboardingPending), want: false},
		{name: "upstream error", err: errors.New("http 503: busy"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTerminalProjectProbeError(tc.err); got != tc.want {
				t.Fatalf("IsTerminalProjectProbeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestClearProjectIDProbeMarkers(t *testing.T) {
	meta := map[string]any{
		MetaProjectIDUnavailable: true,
		MetaProjectIDProbedAt:    int64(1),
		MetaProjectIDProbeErr:    "x",
		"project_id":             "keep-me",
	}
	ClearProjectIDProbeMarkers(meta)
	if _, ok := meta[MetaProjectIDUnavailable]; ok {
		t.Fatal("unavailable should be cleared")
	}
	if _, ok := meta[MetaProjectIDProbedAt]; ok {
		t.Fatal("probed_at should be cleared")
	}
	if _, ok := meta[MetaProjectIDProbeErr]; ok {
		t.Fatal("probe_err should be cleared")
	}
	if meta["project_id"] != "keep-me" {
		t.Fatalf("project_id mutated: %#v", meta["project_id"])
	}
}
