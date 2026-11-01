package contentmoderation

import (
	"fmt"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestLogLimiterSpacesLinesPerKey(t *testing.T) {
	var limiter logLimiter
	start := time.Unix(1000, 0)
	if ok, _ := limiter.allow("a", 10*time.Second, start); !ok {
		t.Fatal("the first line of a key goes out")
	}
	for i := 1; i <= 3; i++ {
		if ok, _ := limiter.allow("a", 10*time.Second, start.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("line %d came within the interval", i)
		}
	}
	if ok, _ := limiter.allow("b", 10*time.Second, start.Add(time.Second)); !ok {
		t.Fatal("another key has its own interval")
	}
	ok, dropped := limiter.allow("a", 10*time.Second, start.Add(10*time.Second))
	if !ok || dropped != 3 {
		t.Fatalf("after the interval: ok=%v dropped=%d, want the next line to say it stands for 3", ok, dropped)
	}
	if _, dropped := limiter.allow("a", 10*time.Second, start.Add(21*time.Second)); dropped != 0 {
		t.Fatalf("the count restarts after a line goes out: %d", dropped)
	}
}

func TestLogLimiterDoesNotGrowWithoutBound(t *testing.T) {
	var limiter logLimiter
	now := time.Unix(1000, 0)
	for i := 0; i < maxLimitedLogKeys+500; i++ {
		limiter.allow(fmt.Sprintf("key-%d", i), time.Minute, now)
	}
	if len(limiter.entries) > maxLimitedLogKeys {
		t.Fatalf("%d keys remembered", len(limiter.entries))
	}
	// Once they have aged out there is room again.
	if ok, _ := limiter.allow("fresh", time.Minute, now.Add(2*time.Minute)); !ok {
		t.Fatal("old keys must make room for new ones")
	}
}

func TestWordHitsAreLoggedOncePerTenantAndRule(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	moderator := &RuntimeModerator{}
	verdict := WordVerdict{Action: WordVerdictLog, MatchCount: 1, Matches: []WordMatch{{Layer: WordScopeTenant, RuleID: "r1"}}}
	for i := 0; i < 200; i++ {
		moderator.logWordVerdict("tenant-a", verdict, 100)
	}
	if got := len(hook.AllEntries()); got != 1 {
		t.Fatalf("200 hits of one tenant on one rule wrote %d lines, want 1", got)
	}
	moderator.logWordVerdict("tenant-b", verdict, 100)
	other := verdict
	other.Matches = []WordMatch{{Layer: WordScopeTenant, RuleID: "r2"}}
	moderator.logWordVerdict("tenant-a", other, 100)
	if got := len(hook.AllEntries()); got != 3 {
		t.Fatalf("another tenant and another rule each get a line: %d lines", got)
	}
	blocked := verdict
	blocked.Action, blocked.Blocked = WordVerdictBlock, true
	moderator.logWordVerdict("tenant-a", blocked, 100)
	if got := len(hook.AllEntries()); got != 4 {
		t.Fatalf("a block is not hidden behind the log lines of the same rule: %d lines", got)
	}
}

func TestRegexSkippedIsLoggedOncePerTenant(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	moderator := &RuntimeModerator{}
	for i := 0; i < 50; i++ {
		moderator.logWordVerdict("tenant-a", WordVerdict{Action: WordVerdictAllow, RegexSkipped: true}, 5<<20)
	}
	entries := hook.AllEntries()
	if len(entries) != 1 || entries[0].Level != log.WarnLevel {
		t.Fatalf("entries = %v", entries)
	}
	if entries[0].Data["body_bytes"] != 5<<20 {
		t.Fatalf("the line must say how large the request was: %v", entries[0].Data)
	}
}
