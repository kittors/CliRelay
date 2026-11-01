package usage

import (
	"fmt"
	"testing"
	"time"
)

func recordReading(t *testing.T, subject, key string, at time.Time, remaining float64, reset *time.Time) {
	t.Helper()
	if err := RecordAIAccountSubjectQuotaPoints(subject, "codex", []QuotaSnapshotPoint{{
		RecordedAt: at, Provider: "codex", QuotaKey: key, QuotaLabel: key,
		Percent: &remaining, ResetAt: reset, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestQueryAIAccountSubjectQuotaReadings(t *testing.T) {
	initSharedSubjectTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(48 * time.Hour)

	recordReading(t, "authsub_a", "code_week", now.Add(-8*time.Hour), 90, &reset) // before the cutoff
	recordReading(t, "authsub_a", "code_week", now.Add(-3*time.Hour), 70, &reset)
	recordReading(t, "authsub_a", "code_week", now.Add(-1*time.Hour), 60, nil)
	recordReading(t, "authsub_a", "code_5h", now.Add(-2*time.Hour), 50, &reset) // a window nobody asked for
	recordReading(t, "authsub_b", "code_week", now.Add(-2*time.Hour), 40, &reset)
	recordReading(t, "authsub_other", "code_week", now.Add(-2*time.Hour), 10, &reset) // an account nobody asked for

	got, err := QueryAIAccountSubjectQuotaReadings(
		[]string{"authsub_a", "authsub_b", "authsub_a", " "}, []string{"code_week", "code_week"}, now.Add(-6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("readings = %+v, want 3 (a at -3h, b at -2h, a at -1h)", got)
	}
	// Oldest first, across accounts.
	order := []struct {
		subject   string
		remaining float64
		hasReset  bool
	}{{"authsub_a", 70, true}, {"authsub_b", 40, true}, {"authsub_a", 60, false}}
	for i, want := range order {
		r := got[i]
		if r.AuthSubjectID != want.subject || r.QuotaKey != "code_week" || r.RemainingPercent == nil || *r.RemainingPercent != want.remaining {
			t.Fatalf("reading %d = %+v, want %s at %v%%", i, r, want.subject, want.remaining)
		}
		if want.hasReset != !r.ResetAt.IsZero() {
			t.Fatalf("reading %d reset = %v, want set=%v", i, r.ResetAt, want.hasReset)
		}
		if want.hasReset && !r.ResetAt.Equal(reset) {
			t.Fatalf("reading %d reset = %v, want %v", i, r.ResetAt, reset)
		}
		if r.RecordedAt.IsZero() || r.RecordedAt.Location() != time.UTC {
			t.Fatalf("reading %d recorded at %v, want a UTC time", i, r.RecordedAt)
		}
	}
	if !got[0].RecordedAt.Before(got[1].RecordedAt) || !got[1].RecordedAt.Before(got[2].RecordedAt) {
		t.Fatalf("readings are not oldest first: %v %v %v", got[0].RecordedAt, got[1].RecordedAt, got[2].RecordedAt)
	}
}

func TestQueryAIAccountSubjectQuotaReadingsNamesNothingOrEverything(t *testing.T) {
	initSharedSubjectTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	recordReading(t, "authsub_only", "code_week", now.Add(-time.Hour), 80, nil)

	for name, args := range map[string][2][]string{
		"no accounts": {nil, {"code_week"}},
		"no windows":  {{"authsub_only"}, nil},
	} {
		got, err := QueryAIAccountSubjectQuotaReadings(args[0], args[1], now.Add(-time.Hour*6))
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: readings = %+v, err = %v; want none", name, got, err)
		}
	}

	// More accounts than one query names: the answer is the same as for one.
	ids := []string{"authsub_only"}
	for i := 0; i < quotaReadingsChunk*2+13; i++ {
		ids = append(ids, fmt.Sprintf("authsub_idle_%d", i))
	}
	got, err := QueryAIAccountSubjectQuotaReadings(ids, []string{"code_week"}, now.Add(-time.Hour*6))
	if err != nil || len(got) != 1 || got[0].AuthSubjectID != "authsub_only" {
		t.Fatalf("readings = %+v, err = %v; want the one stored reading", got, err)
	}
}

func TestQueryAIAccountSubjectQuotaReadingsKeepsAMissingPercentAsNil(t *testing.T) {
	initSharedSubjectTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := RecordAIAccountSubjectQuotaPoints("authsub_blank", "codex", []QuotaSnapshotPoint{{
		RecordedAt: now.Add(-time.Hour), Provider: "codex", QuotaKey: "code_week", QuotaLabel: "week", WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := QueryAIAccountSubjectQuotaReadings([]string{"authsub_blank"}, []string{"code_week"}, now.Add(-6*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("readings = %+v, err = %v", got, err)
	}
	if got[0].RemainingPercent != nil {
		t.Fatalf("a probe that gave no percentage was stored as %v, want nil", *got[0].RemainingPercent)
	}
}
