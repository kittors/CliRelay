package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Single-node host operations: the hang watchdog in deploy/single-node and
// the backup scripts in deploy/cluster/backup run as root on the production
// host and on the offsite backup host. These tests drive the shipped scripts
// with fake systemctl, curl, docker and rsync on PATH.

const watchdogScript = "deploy/single-node/clirelay-watchdog.sh"

// watchdogFakeSystemctl answers "show -p <prop> --value <unit>" from files under
// $FAKE_STATE (<unit>.active, <unit>.sub, <unit>.entered as a unix time) and
// records every other call in $FAKE_CALLS.
const watchdogFakeSystemctl = `#!/usr/bin/env bash
if [ "$1" = show ]; then
  prop=""; prev=""
  for a in "$@"; do [ "$prev" = -p ] && prop="$a"; prev="$a"; done
  unit="${@: -1}"
  case "$prop" in
    ActiveState) cat "$FAKE_STATE/$unit.active" 2>/dev/null || echo inactive ;;
    SubState) cat "$FAKE_STATE/$unit.sub" 2>/dev/null || echo dead ;;
    ActiveEnterTimestamp) if [ -f "$FAKE_STATE/$unit.entered" ]; then echo "@$(cat "$FAKE_STATE/$unit.entered")"; else echo ""; fi ;;
    MainPID) echo 4242 ;;
  esac
  exit 0
fi
echo "$*" >> "$FAKE_CALLS"
`

// watchdogFakeReadyz answers a probe of 127.0.0.1:<port>/readyz with the code in
// $FAKE_STATE/readyz-<port>, or 000 (connection refused) when there is none.
const watchdogFakeReadyz = `#!/usr/bin/env bash
url="${@: -1}"; port="${url#http://127.0.0.1:}"; port="${port%%/*}"
cat "$FAKE_STATE/readyz-$port" 2>/dev/null || printf 000
`

type watchdogHarness struct {
	t     *testing.T
	dir   string
	state string
	calls string
	base  string
	bin   string
}

func newWatchdogHarness(t *testing.T) *watchdogHarness {
	t.Helper()
	dir := t.TempDir()
	h := &watchdogHarness{t: t, dir: dir, state: filepath.Join(dir, "state"), calls: filepath.Join(dir, "calls"),
		base: filepath.Join(dir, "base"), bin: filepath.Join(dir, "bin")}
	for _, d := range []string{h.state, h.base, h.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(h.bin, "systemctl"), watchdogFakeSystemctl)
	writeExecutable(t, filepath.Join(h.bin, "curl"), watchdogFakeReadyz)
	return h
}

// slot sets a unit's state; enteredAgo is how long ago it became active.
func (h *watchdogHarness) slot(port int, active, sub string, enteredAgo time.Duration) {
	h.t.Helper()
	unit := filepath.Join(h.state, fmt.Sprintf("clirelay2-%d", port))
	h.write(unit+".active", active)
	h.write(unit+".sub", sub)
	h.write(unit+".entered", strconv.FormatInt(time.Now().Add(-enteredAgo).Unix(), 10))
}

func (h *watchdogHarness) readyz(port int, code string) {
	h.write(filepath.Join(h.state, fmt.Sprintf("readyz-%d", port)), code)
}

func (h *watchdogHarness) activePort(value string) {
	h.write(filepath.Join(h.base, ".active-port"), value)
}

func (h *watchdogHarness) write(path, content string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// ticks sources the shipped script and runs watchdog_tick n times, running
// the shell snippet between after each tick; it returns the systemctl calls
// other than show.
func (h *watchdogHarness) ticks(n int, between string) string {
	h.t.Helper()
	_ = os.Remove(h.calls)
	if between == "" {
		between = ":"
	}
	script := fmt.Sprintf("source %s\nBASE_DIR=%q\nfor i in $(seq %d); do watchdog_tick; %s; done\n",
		watchdogScript, h.base, n, between)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FAKE_STATE="+h.state, "FAKE_CALLS="+h.calls)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("watchdog ticks: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(h.calls)
	return string(calls)
}

func TestWatchdogKillsOnlyAHungActiveSlot(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8319")
	h.slot(8319, "active", "running", time.Hour)
	h.readyz(8319, "204")
	if calls := h.ticks(3, ""); calls != "" {
		t.Fatalf("a healthy active slot must be left alone, got %q", calls)
	}

	h.readyz(8319, "503")
	if calls := h.ticks(1, ""); calls != "" {
		t.Fatalf("one failed probe must not kill, got %q", calls)
	}
	if calls := h.ticks(2, ""); calls != "kill -s KILL clirelay2-8319\n" {
		t.Fatalf("two failed probes in a row must SIGKILL the active slot once, got %q", calls)
	}
}

// The 2026-10-27 incident: after cutover to 8318 the drain stops 8319, whose
// listener is already closed; the old watchdog SIGKILLed it and started it
// again on the retired build.
func TestWatchdogLeavesTheRetiredSlotAlone(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", time.Hour)
	h.readyz(8318, "204")
	h.slot(8319, "active", "running", 2*time.Hour) // draining: listener closed
	h.readyz(8319, "000")
	if calls := h.ticks(4, ""); calls != "" {
		t.Fatalf("a draining old slot must not be touched, got %q", calls)
	}
	h.slot(8319, "inactive", "dead", 2*time.Hour)
	if calls := h.ticks(4, ""); calls != "" {
		t.Fatalf("a stopped old slot must stay stopped, got %q", calls)
	}
}

func TestWatchdogResetsWhenTheActiveSlotChanges(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8319")
	h.slot(8319, "active", "running", time.Hour)
	h.slot(8318, "active", "running", time.Hour)
	h.readyz(8319, "503")
	h.readyz(8318, "503")
	// one failure on 8319, then the deploy moves traffic to 8318
	calls := h.ticks(2, fmt.Sprintf("echo 8318 > %q", filepath.Join(h.base, ".active-port")))
	if calls != "" {
		t.Fatalf("failures on the previous slot must not count against the new one, got %q", calls)
	}
}

func TestWatchdogWaitsOutStartupAndCrashes(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", 10*time.Second)
	h.readyz(8318, "000")
	if calls := h.ticks(3, ""); calls != "" {
		t.Fatalf("a slot inside its startup grace must not be killed, got %q", calls)
	}
	h.slot(8318, "activating", "auto-restart", time.Hour)
	if calls := h.ticks(3, ""); calls != "" {
		t.Fatalf("a crashed slot belongs to Restart=always, got %q", calls)
	}
	for _, port := range []string{"garbage", ""} {
		h.activePort(port)
		if calls := h.ticks(3, ""); calls != "" {
			t.Fatalf("active-port %q must be ignored, got %q", port, calls)
		}
	}
}

func TestWatchdogNeverStartsAUnit(t *testing.T) {
	data, err := os.ReadFile(watchdogScript)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		if strings.Contains(code, "systemctl start") || strings.Contains(code, "systemctl restart") {
			t.Fatalf("the watchdog must only SIGKILL (Restart=always brings the slot back): %s", code)
		}
	}
	unit, err := os.ReadFile("deploy/single-node/clirelay-watchdog.service")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "clirelay2-831") {
		t.Fatalf("the watchdog unit must not be tied to one slot:\n%s", unit)
	}
}
