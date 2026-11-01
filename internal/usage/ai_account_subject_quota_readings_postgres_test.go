package usage_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// TestPostgresQuotaReadings runs the readings query on PostgreSQL, where
// recorded_at and reset_at are timestamptz columns and the cutoff arrives as a
// text parameter: the comparison, the oldest-first order, the nil percentage and
// the chunking over many accounts must all behave as on SQLite.
func TestPostgresQuotaReadings(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	db := usage.RuntimeDB()

	prefix := "readings-" + uuid.NewString()[:8] + "-"
	subjectA, subjectB, subjectIdle := prefix+"a", prefix+"b", prefix+"idle"
	// PostgreSQL references the account's subject row from the readings and from
	// the cycle row a reading maintains, so the subjects must exist first.
	for _, subject := range []string{subjectA, subjectB} {
		if err := usage.UpsertAIAccountSubject(&usage.AuthSubjectIdentity{
			ID: subject, Provider: "codex", SubjectScope: "tenant", SeedKind: "test", SeedHash: subject,
		}); err != nil {
			t.Fatalf("create subject %s: %v", subject, err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"ai_account_subject_quota_points", "ai_account_subject_quota_cycles", "ai_account_subjects"} {
			_, _ = db.Exec(`DELETE FROM `+table+` WHERE auth_subject_id LIKE $1`, prefix+"%")
		}
	})

	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(48 * time.Hour)
	record := func(subject, key string, at time.Time, remaining *float64, resetAt *time.Time) {
		t.Helper()
		if err := usage.RecordAIAccountSubjectQuotaPoints(subject, "codex", []usage.QuotaSnapshotPoint{{
			RecordedAt: at, Provider: "codex", QuotaKey: key, QuotaLabel: key,
			Percent: remaining, ResetAt: resetAt, WindowSeconds: 604800,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	pct := func(v float64) *float64 { return &v }

	record(subjectA, "code_week", now.Add(-8*time.Hour), pct(90), &reset) // before the cutoff
	record(subjectA, "code_week", now.Add(-3*time.Hour), pct(70), &reset)
	record(subjectA, "code_week", now.Add(-1*time.Hour), pct(60), nil)
	record(subjectA, "code_5h", now.Add(-2*time.Hour), pct(50), &reset) // a window nobody asked for
	record(subjectB, "code_week", now.Add(-2*time.Hour), pct(40), &reset)
	record(subjectB, "code_week", now.Add(-30*time.Minute), nil, nil) // the probe gave no percentage

	got, err := usage.QueryAIAccountSubjectQuotaReadings(
		[]string{subjectA, subjectB, subjectA, " "}, []string{"code_week", "code_week"}, now.Add(-6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("readings = %+v, want 4 (a at -3h, b at -2h, a at -1h, b at -30m)", got)
	}
	order := []struct {
		subject   string
		remaining *float64
		hasReset  bool
	}{{subjectA, pct(70), true}, {subjectB, pct(40), true}, {subjectA, pct(60), false}, {subjectB, nil, false}}
	for i, want := range order {
		r := got[i]
		if r.AuthSubjectID != want.subject || r.QuotaKey != "code_week" {
			t.Fatalf("reading %d = %+v, want %s", i, r, want.subject)
		}
		if (want.remaining == nil) != (r.RemainingPercent == nil) || (want.remaining != nil && *want.remaining != *r.RemainingPercent) {
			t.Fatalf("reading %d percent = %v, want %v", i, r.RemainingPercent, want.remaining)
		}
		if want.hasReset != !r.ResetAt.IsZero() || (want.hasReset && !r.ResetAt.Equal(reset)) {
			t.Fatalf("reading %d reset = %v, want set=%v (%v)", i, r.ResetAt, want.hasReset, reset)
		}
		if r.RecordedAt.IsZero() {
			t.Fatalf("reading %d has no recording time", i)
		}
		if i > 0 && r.RecordedAt.Before(got[i-1].RecordedAt) {
			t.Fatalf("readings are not oldest first: %v before %v", r.RecordedAt, got[i-1].RecordedAt)
		}
	}
	if want := now.Add(-3 * time.Hour); !got[0].RecordedAt.Equal(want) {
		t.Fatalf("first reading recorded at %v, want %v: the time must survive the round trip", got[0].RecordedAt, want)
	}

	// More accounts than one query names: the answer is the same as for one.
	ids := []string{subjectA, subjectB}
	for i := 0; i < 2*200+13; i++ {
		ids = append(ids, fmt.Sprintf("%sidle-%d", subjectIdle, i))
	}
	again, err := usage.QueryAIAccountSubjectQuotaReadings(ids, []string{"code_week"}, now.Add(-6*time.Hour))
	if err != nil || len(again) != 4 {
		t.Fatalf("with many accounts: readings = %d, err = %v; want the same 4", len(again), err)
	}
}
