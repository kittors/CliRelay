package alerting

import (
	"strings"
	"testing"
	"time"
)

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

func window(key string, span time.Duration, used float64, resetIn time.Duration, observedAgo time.Duration) WindowSample {
	now := timeNow()
	w := WindowSample{Key: key, Label: key, Span: span, UsedPercent: used}
	if resetIn != 0 {
		w.ResetAt = now.Add(resetIn)
	}
	if observedAgo >= 0 {
		w.ObservedAt = now.Add(-observedAgo)
	}
	return w
}

func account(id string, windows ...WindowSample) AccountSample {
	return AccountSample{ID: id, Subject: "subject-" + id, Provider: "codex", Name: id + "@example.com", Windows: windows}
}

func TestQuotaCandidates(t *testing.T) {
	now := timeNow()
	rule := QuotaWindowRule{Enabled: true, ThresholdPercent: 90}

	t.Run("fires at the threshold and above", func(t *testing.T) {
		got := quotaCandidates(rule, []AccountSample{
			account("a", window("code_week", 7*day, 90, 2*day, 0)),
			account("b", window("code_week", 7*day, 89.9, 2*day, 0)),
			account("c", window("code_week", 7*day, 100, 2*day, 0)),
		}, now)
		if len(got) != 2 {
			t.Fatalf("candidates = %d, want 2", len(got))
		}
		bySubject := map[string]Candidate{}
		for _, c := range got {
			bySubject[c.Subject.ID] = c
		}
		if bySubject["subject-a"].Severity != SeverityWarning || bySubject["subject-c"].Severity != SeverityCritical {
			t.Errorf("severities: %+v", bySubject)
		}
	})

	t.Run("the key holds no reset time", func(t *testing.T) {
		a := quotaCandidates(rule, []AccountSample{account("a", window("code_week", 7*day, 95, 2*day, 0))}, now)
		b := quotaCandidates(rule, []AccountSample{account("a", window("code_week", 7*day, 95, 2*day+2*time.Second, 0))}, now)
		if len(a) != 1 || len(b) != 1 || a[0].Key != b[0].Key {
			t.Fatalf("a reset time that drifts by seconds must not change the key: %q vs %q", a[0].Key, b[0].Key)
		}
	})

	t.Run("short windows are left alone, unknown ones are not", func(t *testing.T) {
		got := quotaCandidates(rule, []AccountSample{account("a",
			window("five_hour", 5*hour, 99, 2*hour, 0),
			window("daily", 24*hour, 95, 2*hour, 0),
			window("model:x", 0, 95, 3*day, 0),
		)}, now)
		keys := map[string]bool{}
		for _, c := range got {
			keys[c.Facts["window_key"].(string)] = true
		}
		if keys["five_hour"] || !keys["daily"] || !keys["model:x"] {
			t.Errorf("windows reported: %v", keys)
		}
	})

	t.Run("a window that has renewed since it was read says nothing", func(t *testing.T) {
		got := quotaCandidates(rule, []AccountSample{account("a", window("code_week", 7*day, 99, -time.Minute, 0))}, now)
		if len(got) != 0 {
			t.Fatalf("a stale reading of a renewed window was reported: %+v", got)
		}
	})

	t.Run("credentials of one account are one alert", func(t *testing.T) {
		first := account("a", window("code_week", 7*day, 95, 2*day, 0))
		second := first
		second.ID = "a-second-file"
		got := quotaCandidates(rule, []AccountSample{first, second}, now)
		if len(got) != 1 {
			t.Fatalf("candidates = %d, want 1", len(got))
		}
	})

	t.Run("the text says what is used, what is left and when it resets", func(t *testing.T) {
		got := quotaCandidates(rule, []AccountSample{account("a", window("code_week", 7*day, 91.7, 2*day+3*hour, 0))}, now)
		en := got[0].Describe(LanguageEN)
		for _, want := range []string{"codex · a@example.com", "weekly window", "91% used", "9% left", "resets in 2d 3h", "2026-11-04 15:00 UTC"} {
			if !strings.Contains(en, want) {
				t.Errorf("English text %q lacks %q", en, want)
			}
		}
		zh := got[0].Describe(LanguageZHCN)
		for _, want := range []string{"每周窗口", "已用 91%", "剩余 9%", "2 天 3 小时", "2026-11-04 15:00 UTC"} {
			if !strings.Contains(zh, want) {
				t.Errorf("Chinese text %q lacks %q", zh, want)
			}
		}
	})
}

func TestUseItOrLoseItCandidates(t *testing.T) {
	now := timeNow()
	rule := UseItOrLoseItRule{Enabled: true, MinRemainingPercent: 40, HoursBeforeReset: 24}
	fresh := time.Hour

	tests := []struct {
		name string
		acc  AccountSample
		want int
	}{
		{"lots left, resets within a day", account("a", window("code_week", 7*day, 50, 10*hour, fresh)), 1},
		{"exactly the limits", account("a", window("code_week", 7*day, 60, 24*hour, fresh)), 1},
		{"not enough left", account("a", window("code_week", 7*day, 70, 10*hour, fresh)), 0},
		{"reset is too far", account("a", window("code_week", 7*day, 20, 25*hour, fresh)), 0},
		{"short window", account("a", window("five_hour", 5*hour, 10, 2*hour, fresh)), 0},
		{"reset unknown", account("a", window("code_week", 7*day, 10, 0, fresh)), 0},
		{"already renewed", account("a", window("code_week", 7*day, 10, -hour, fresh)), 0},
		{"reading too old", account("a", window("code_week", 7*day, 10, 10*hour, 7*hour)), 0},
		{"reading age unknown", account("a", window("code_week", 7*day, 10, 10*hour, -1)), 0},
		{"a monthly window counts as long", account("a", window("month", 30*day, 10, 10*hour, fresh)), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useItOrLoseItCandidates(rule, []AccountSample{tt.acc}, now); len(got) != tt.want {
				t.Fatalf("candidates = %d, want %d", len(got), tt.want)
			}
		})
	}

	t.Run("an account that is not working cannot use it", func(t *testing.T) {
		acc := account("a", window("code_week", 7*day, 10, 10*hour, fresh))
		acc.Failure = FailureSample{Class: FailureRevoked, Reason: "reauth_required"}
		if got := useItOrLoseItCandidates(rule, []AccountSample{acc}, now); len(got) != 0 {
			t.Fatalf("candidates = %d, want 0", len(got))
		}
	})

	t.Run("the text tells the operator what to do", func(t *testing.T) {
		got := useItOrLoseItCandidates(rule, []AccountSample{account("a", window("code_week", 7*day, 38, 9*hour+20*time.Minute, fresh))}, now)
		text := got[0].Describe(LanguageEN)
		for _, want := range []string{"62% left", "resets in 9h 20m", "Use it before it is lost"} {
			if !strings.Contains(text, want) {
				t.Errorf("text %q lacks %q", text, want)
			}
		}
		if got[0].Severity != SeverityInfo {
			t.Errorf("severity = %q", got[0].Severity)
		}
	})
}

func failing(id string, class FailureClass, reason string) AccountSample {
	a := account(id)
	a.Failure = FailureSample{Class: class, Reason: reason, HTTPStatus: 401}
	return a
}

func TestFailureCandidates(t *testing.T) {
	rule := AccountFailureRule{Enabled: true, ForMinutes: 10}
	now := timeNow()
	since := map[string]time.Time{}

	// First sight: only a revoked credential is reported at once.
	accounts := []AccountSample{
		failing("revoked", FailureRevoked, "reauth_required"),
		failing("rejected", FailureNeedsAction, "unauthorized"),
		failing("flaky", FailureTransient, "upstream_error"),
		account("healthy"),
	}
	got, active := failureCandidates(rule, accounts, since, now)
	if len(got) != 1 || got[0].Subject.ID != "revoked" || got[0].Severity != SeverityCritical {
		t.Fatalf("first round: %+v", got)
	}
	if len(active) != 3 {
		t.Errorf("active keys = %v, want the three failing accounts", active)
	}

	// Nine minutes later: still waiting.
	got, _ = failureCandidates(rule, accounts, since, now.Add(9*time.Minute))
	if len(got) != 1 {
		t.Fatalf("after 9 minutes: %d candidates, want only the revoked one", len(got))
	}

	// Ten minutes: the ones that need to last have lasted.
	got, _ = failureCandidates(rule, accounts, since, now.Add(10*time.Minute))
	if len(got) != 3 {
		t.Fatalf("after 10 minutes: %d candidates, want 3", len(got))
	}
	for _, c := range got {
		if c.Key != failureKeyPrefix+c.Subject.ID {
			t.Errorf("key = %q", c.Key)
		}
		if c.Facts["failing_since"] != now.UTC().Format(time.RFC3339) {
			t.Errorf("failing_since = %v, want the first sight", c.Facts["failing_since"])
		}
	}

	// A recovered account is forgotten, so its next failure starts a new clock.
	accounts[1] = account("rejected")
	_, _ = failureCandidates(rule, accounts, since, now.Add(11*time.Minute))
	if _, kept := since["rejected"]; kept {
		t.Error("a recovered account kept its clock")
	}
	accounts[1] = failing("rejected", FailureNeedsAction, "unauthorized")
	got, _ = failureCandidates(rule, accounts, since, now.Add(12*time.Minute))
	for _, c := range got {
		if c.Subject.ID == "rejected" {
			t.Fatal("a failure that just started was reported before it lasted")
		}
	}

	t.Run("zero minutes reports at once", func(t *testing.T) {
		got, _ := failureCandidates(AccountFailureRule{Enabled: true, ForMinutes: 0}, []AccountSample{failing("x", FailureTransient, "upstream_error")}, map[string]time.Time{}, now)
		if len(got) != 1 {
			t.Fatalf("candidates = %d", len(got))
		}
	})

	t.Run("the text explains the reason", func(t *testing.T) {
		got, _ := failureCandidates(rule, []AccountSample{failing("x", FailureRevoked, "reauth_required")}, map[string]time.Time{}, now)
		text := got[0].Describe(LanguageEN)
		if !strings.Contains(text, "sign the account in again") || !strings.Contains(text, "codex · x@example.com") {
			t.Errorf("text = %q", text)
		}
		if zh := got[0].Describe(LanguageZHCN); !strings.Contains(zh, "重新登录") {
			t.Errorf("text = %q", zh)
		}
	})
}

func TestForgetFailures(t *testing.T) {
	state := SentState{Records: map[string]SentRecord{
		failureKeyPrefix + "still": {Until: timeNow().Add(hour)},
		failureKeyPrefix + "gone":  {Until: timeNow().Add(hour)},
		"quota_window|x|y":         {Until: timeNow().Add(hour)},
	}}
	state.forgetFailures(failureKeyPrefix, map[string]struct{}{failureKeyPrefix + "still": {}})
	if _, ok := state.Records[failureKeyPrefix+"gone"]; ok {
		t.Error("the record of a recovered account survived")
	}
	if _, ok := state.Records[failureKeyPrefix+"still"]; !ok {
		t.Error("the record of an account that is still failing was dropped")
	}
	if _, ok := state.Records["quota_window|x|y"]; !ok {
		t.Error("another rule's record was dropped")
	}
}

func TestSpendingCandidates(t *testing.T) {
	now := timeNow()
	rule := SpendingRule{Enabled: true, ThresholdPercent: 90}
	reset := now.Add(2*day + 3*hour)
	spenders := []SpendSample{
		{Kind: SubjectKey, ID: "k1", Name: "dev team", Cells: []SpendCell{
			{Metric: "cost", Window: "week", Used: 46, Limit: 50, ResetAt: reset},
			{Metric: "requests", Window: "day", Used: 10, Limit: 1000, ResetAt: now.Add(hour)},
			{Metric: "tokens", Window: "lifetime", Used: 950000, Limit: 1000000},
			{Metric: "cost", Window: "month", Used: 5, Limit: 0},
		}},
		{Kind: SubjectEndUser, ID: "u1", Name: "Alice", Cells: []SpendCell{
			{Metric: "cost", Window: "5h", Used: 12, Limit: 10, ResetAt: now.Add(hour)},
		}},
	}
	got := spendingCandidates(rule, spenders, now)
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3: %+v", len(got), got)
	}
	byKey := map[string]Candidate{}
	for _, c := range got {
		byKey[c.Key] = c
	}
	week := byKey["spending|key|k1|cost|week|50"]
	if week.Severity != SeverityWarning || week.Until != reset {
		t.Errorf("weekly cost candidate = %+v", week)
	}
	text := week.Describe(LanguageEN)
	for _, want := range []string{`API key "dev team"`, "92% of the weekly spending limit", "$46.00 of $50.00", "resets in 2d 3h"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q lacks %q", text, want)
		}
	}
	if zh := week.Describe(LanguageZHCN); !strings.Contains(zh, "每周花费限额已用 92%") {
		t.Errorf("Chinese text = %q", zh)
	}
	lifetime := byKey["spending|key|k1|tokens|lifetime|1000000"]
	if lifetime.Key == "" {
		for k := range byKey {
			t.Logf("key: %s", k)
		}
		t.Fatal("no lifetime candidate")
	}
	if text := lifetime.Describe(LanguageEN); !strings.Contains(text, "950K of 1M") || !strings.Contains(text, "does not reset") {
		t.Errorf("lifetime text = %q", text)
	}
	over := byKey["spending|end_user|u1|cost|5h|10"]
	if over.Severity != SeverityCritical {
		t.Errorf("a limit that is exceeded is critical: %+v", over)
	}

	t.Run("changing the limit makes it a new problem", func(t *testing.T) {
		changed := []SpendSample{{Kind: SubjectKey, ID: "k1", Name: "x", Cells: []SpendCell{{Metric: "cost", Window: "week", Used: 46, Limit: 51, ResetAt: reset}}}}
		again := spendingCandidates(SpendingRule{Enabled: true, ThresholdPercent: 80}, changed, now)
		if len(again) != 1 || again[0].Key == week.Key {
			t.Fatalf("keys: %+v vs %q", again, week.Key)
		}
	})
}

func TestCleanTextMasksSecretsAndControlCharacters(t *testing.T) {
	got := cleanText("my key\nsk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ\x00 done")
	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") || strings.ContainsAny(got, "\n\x00") {
		t.Errorf("cleanText = %q", got)
	}
	long := cleanText(strings.Repeat("x", 5000))
	if len([]rune(long)) > maxScrubbedText {
		t.Errorf("length = %d", len([]rune(long)))
	}
}

func TestFormatting(t *testing.T) {
	if got := usedPercent(99.6); got != "99%" {
		t.Errorf("usedPercent(99.6) = %q: a window that is not exhausted must not read 100%%", got)
	}
	if got := leftPercent(0.4); got != "1%" {
		t.Errorf("leftPercent(0.4) = %q", got)
	}
	for in, want := range map[time.Duration]string{
		30 * time.Second:        "under a minute",
		12 * time.Minute:        "12m",
		5*hour + 30*time.Minute: "5h 30m",
		3 * hour:                "3h",
		2*day + 3*hour:          "2d 3h",
		2 * day:                 "2d",
	} {
		if got := formatDuration(in, LanguageEN); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", in, got, want)
		}
	}
	if got := formatDuration(2*day+3*hour, LanguageZHCN); got != "2 天 3 小时" {
		t.Errorf("zh = %q", got)
	}
	for in, want := range map[float64]string{5: "5", 12345: "12.3K", 1234567: "1.23M", 2_500_000_000: "2.5B"} {
		if got := compactNumber(in); got != want {
			t.Errorf("compactNumber(%v) = %q, want %q", in, got, want)
		}
	}
}
