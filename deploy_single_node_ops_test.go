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

// watchdogFakeCurl answers a GET of 127.0.0.1:<port>/<path> from
// $FAKE_STATE/<path>-<port>: "<status> [reason]" is an HTTP answer (a reason
// comes back in the JSON body /readyz sends), while "refused", "timeout",
// "empty", "reset" and "broken" fail the way curl does (exit 7, 28, 52, 56
// and 2) with status 000. A missing file is a refused connection. Every probe
// is logged to $FAKE_STATE/curl.log as "<path> <port> <timeout>".
const watchdogFakeCurl = `#!/usr/bin/env bash
set -f
fmt=""; out=""; timeout=""
while [ $# -gt 1 ]; do
  case "$1" in
    -w) fmt="$2"; shift ;;
    -o) out="$2"; shift ;;
    -m) timeout="$2"; shift ;;
  esac
  shift
done
rest="${1#http://127.0.0.1:}"; port="${rest%%/*}"; path="${rest#*/}"
echo "$path $port $timeout" >> "$FAKE_STATE/curl.log"
reply="$(cat "$FAKE_STATE/$path-$port" 2>/dev/null || echo refused)"
set -- $reply
body=""
case "$1" in
  refused) code=000 rc=7 ;;
  timeout) code=000 rc=28 ;;
  empty) code=000 rc=52 ;;
  reset) code=000 rc=56 ;;
  broken) code=000 rc=2 ;;
  *) code="$1" rc=0
     if [ -n "${2:-}" ]; then body="{\"in_flight_count\":0,\"reason\":\"$2\",\"status\":\"not_ready\"}"; fi ;;
esac
if [ -n "$out" ]; then printf '%s' "$body" > "$out"; else printf '%s' "$body"; fi
printf '%b' "${fmt//'%{http_code}'/$code}"
exit "$rc"
`

type watchdogHarness struct {
	t         *testing.T
	dir       string
	state     string
	calls     string
	base      string
	bin       string
	snapshots string
	// output is what the watchdog logged during the last ticks call.
	output string
}

func newWatchdogHarness(t *testing.T) *watchdogHarness {
	t.Helper()
	dir := t.TempDir()
	h := &watchdogHarness{t: t, dir: dir, state: filepath.Join(dir, "state"), calls: filepath.Join(dir, "calls"),
		base: filepath.Join(dir, "base"), bin: filepath.Join(dir, "bin"), snapshots: filepath.Join(dir, "snapshots")}
	for _, d := range []string{h.state, h.base, h.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(h.bin, "systemctl"), watchdogFakeSystemctl)
	writeExecutable(t, filepath.Join(h.bin, "curl"), watchdogFakeCurl)
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

func (h *watchdogHarness) replyFile(path string, port int) string {
	return filepath.Join(h.state, fmt.Sprintf("%s-%d", path, port))
}

// reply sets what GET /<path> on the slot's port gets back (see watchdogFakeCurl).
func (h *watchdogHarness) reply(path string, port int, reply string) {
	h.write(h.replyFile(path, port), reply)
}

// after returns a snippet for ticks that, once round k has run, makes
// GET /<path> on port get reply back from then on.
func (h *watchdogHarness) after(k int, path string, port int, reply string) string {
	return fmt.Sprintf(`if [ "$i" = %d ]; then echo %q > %q; fi`, k, reply, h.replyFile(path, port))
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
// the shell snippet between after each tick (round number in $i); it returns
// the systemctl calls other than show. Each call starts a fresh watchdog, so
// counts do not carry over from the previous call.
func (h *watchdogHarness) ticks(n int, between string) string {
	h.t.Helper()
	_ = os.Remove(h.calls)
	_ = os.Remove(filepath.Join(h.state, "curl.log"))
	if between == "" {
		between = ":"
	}
	script := fmt.Sprintf("source %s || exit 97\nBASE_DIR=%q\nSNAPSHOT_DIR=%q\nfor i in $(seq %d); do watchdog_tick; %s; done\nexit 0\n",
		watchdogScript, h.base, h.snapshots, n, between)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FAKE_STATE="+h.state, "FAKE_CALLS="+h.calls)
	out, err := cmd.CombinedOutput()
	h.output = string(out)
	if err != nil {
		h.t.Fatalf("watchdog ticks: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(h.calls)
	return string(calls)
}

// probes returns the probes the last ticks call sent, one
// "<path> <port> <timeout>" line each.
func (h *watchdogHarness) probes() string {
	data, _ := os.ReadFile(filepath.Join(h.state, "curl.log"))
	return string(data)
}

func TestWatchdogKillsASlotThatStopsAnswering(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8319")
	h.slot(8319, "active", "running", time.Hour)
	h.reply("readyz", 8319, "204")
	if calls := h.ticks(3, ""); calls != "" {
		t.Fatalf("a healthy active slot must be left alone, got %q", calls)
	}
	if probes := h.probes(); probes != strings.Repeat("readyz 8319 8\n", 3) {
		t.Fatalf("a ready slot needs one /readyz probe a round, got %q", probes)
	}
	if h.output != "" {
		t.Fatalf("a ready slot must not be logged every round, got:\n%s", h.output)
	}

	h.reply("readyz", 8319, "timeout")
	h.reply("healthz", 8319, "timeout")
	if calls := h.ticks(3, ""); calls != "" {
		t.Fatalf("three rounds without an answer are under WATCHDOG_FAILS=4, got %q", calls)
	}
	if probes := h.probes(); probes != strings.Repeat("readyz 8319 8\nhealthz 8319 15\n", 3) {
		t.Fatalf("a silent /readyz must be followed by /healthz with the liveness timeout, got %q", probes)
	}
	if !strings.Contains(h.output, "clirelay2-8319: /readyz sent nothing in 8s, /healthz sent nothing in 15s; no HTTP answer (3/4)") {
		t.Fatalf("each silent round must be logged with what both probes saw, got:\n%s", h.output)
	}
	if calls := h.ticks(4, ""); calls != "kill -s KILL clirelay2-8319\n" {
		t.Fatalf("four rounds in a row without any HTTP answer must SIGKILL the active slot once, got %q", calls)
	}

	snaps, err := filepath.Glob(filepath.Join(h.snapshots, "hang-*-8319.txt"))
	if err != nil || len(snaps) != 1 {
		t.Fatalf("a SIGKILL must leave one snapshot, got %v (%v)", snaps, err)
	}
	snap, err := os.ReadFile(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"unit clirelay2-8319 pid 4242", "why no HTTP answer for 4 rounds in a row", "--- cpu.stat", "--- process"} {
		if !strings.Contains(string(snap), want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, snap)
		}
	}
	if !strings.Contains(h.output, "clirelay2-8319 looks hung (pid 4242): no HTTP answer for 4 rounds in a row; SIGKILL, Restart=always brings it back (state saved to "+snaps[0]+")") {
		t.Fatalf("the kill must be logged with its reason and the snapshot, got:\n%s", h.output)
	}

	// a connection the slot accepted and then closed or reset without a reply
	h.reply("readyz", 8319, "empty")
	h.reply("healthz", 8319, "reset")
	if calls := h.ticks(4, ""); calls != "kill -s KILL clirelay2-8319\n" {
		t.Fatalf("connections dropped without an answer count as a hang, got %q", calls)
	}
}

// The 2026-11-16 incident: a slot pinned on CPU answered /readyz with 503
// "postgres" (its 2 s PostgreSQL ping timed out) to four probes in a row, and
// the watchdog SIGKILLed it at 17:06 with every request in flight.
func TestWatchdogLeavesASlowSlotAlone(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", time.Hour)
	h.reply("readyz", 8318, "503 postgres")
	if calls := h.ticks(10, ""); calls != "" {
		t.Fatalf("a slot that answers 503 is alive and must not be killed, got %q", calls)
	}
	if !strings.Contains(h.output, "clirelay2-8318: /readyz answered 503 (postgres); alive, not counted as a hang") {
		t.Fatalf("a 503 must be logged with its reason, got:\n%s", h.output)
	}
	if probes := h.probes(); strings.Contains(probes, "healthz") {
		t.Fatalf("an answer from /readyz already shows the process is alive; /healthz must not be asked, got %q", probes)
	}
	for _, status := range []string{"500", "502", "404"} {
		h.reply("readyz", 8318, status)
		if calls := h.ticks(6, ""); calls != "" {
			t.Fatalf("a slot that answers %s is alive and must not be killed, got %q", status, calls)
		}
	}

	// /readyz gives up waiting on the database; /healthz still answers
	h.reply("readyz", 8318, "timeout")
	h.reply("healthz", 8318, "204")
	if calls := h.ticks(10, ""); calls != "" {
		t.Fatalf("a slot whose /healthz answers is slow, not hung, got %q", calls)
	}
	if !strings.Contains(h.output, "clirelay2-8318: /readyz sent nothing in 8s, /healthz answered 204; slow but alive, not counted as a hang") {
		t.Fatalf("a slow round must be logged, got:\n%s", h.output)
	}

	// three silent rounds, one slow, three silent: an answer clears the count
	h.reply("healthz", 8318, "timeout")
	between := h.after(3, "healthz", 8318, "204") + "; " + h.after(4, "healthz", 8318, "timeout")
	if calls := h.ticks(7, between); calls != "" {
		t.Fatalf("an answer in between must clear the count of silent rounds, got %q", calls)
	}
}

// A slot can drain while it is still the active one: after a SIGTERM that did
// not come from systemd the unit stays active/running while
// http.Server.Shutdown closes the listener and waits out
// CLIRELAY_SHUTDOWN_GRACE (300 s) and the shutdown work after it (90 s).
func TestWatchdogWaitsOutADrain(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8319")
	h.slot(8319, "active", "running", time.Hour)
	// ready, then 503 "draining" until Shutdown closes the listener, then refused
	drain := h.after(1, "readyz", 8319, "503 draining") + "; " + h.after(2, "readyz", 8319, "refused")
	h.reply("readyz", 8319, "204")
	// 2 answered rounds, then 13 refused ones: 390 s at the default 30 s interval
	if calls := h.ticks(15, drain); calls != "" {
		t.Fatalf("a slot draining for 300 s + 90 s must not be killed, got %q", calls)
	}
	for _, want := range []string{
		"clirelay2-8319: /readyz answered 503 (draining); alive, not counted as a hang",
		"clirelay2-8319: /readyz refused the connection; nothing listens on 8319 (13/20)",
	} {
		if !strings.Contains(h.output, want) {
			t.Fatalf("missing %q in:\n%s", want, h.output)
		}
	}
	if probes := h.probes(); strings.Contains(probes, "healthz") {
		t.Fatalf("a refused /readyz needs no /healthz probe on the same port, got %q", probes)
	}

	// a process that closed its listener and never exited
	h.reply("readyz", 8319, "204")
	if calls := h.ticks(22, drain); calls != "kill -s KILL clirelay2-8319\n" {
		t.Fatalf("WATCHDOG_REFUSED_FAILS=20 refusals in a row must SIGKILL the slot once, got %q", calls)
	}

	// a stop systemd sent drains as deactivating: never touched
	h.slot(8319, "deactivating", "stop-sigterm", time.Hour)
	h.reply("readyz", 8319, "refused")
	if calls := h.ticks(25, ""); calls != "" {
		t.Fatalf("a slot systemd is stopping belongs to its TimeoutStopSec, got %q", calls)
	}
	if probes := h.probes(); probes != "" {
		t.Fatalf("a slot systemd is stopping must not even be probed, got %q", probes)
	}
}

// A cold start that outlasts WATCHDOG_STARTUP_GRACE may not listen yet;
// killing it would only start it over.
func TestWatchdogLeavesASlotThatNeverAnsweredAlone(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", time.Hour)
	h.reply("readyz", 8318, "refused")
	if calls := h.ticks(30, ""); calls != "" {
		t.Fatalf("a run that has never answered must not be killed for refusing, got %q", calls)
	}
	if n := strings.Count(h.output, "clirelay2-8318: /readyz refused the connection; it has not answered since it started, left alone"); n != 1 {
		t.Fatalf("a run that never answered is logged once, got %d lines:\n%s", n, h.output)
	}

	// once this run has answered, refusals count
	h.reply("readyz", 8318, "204")
	if calls := h.ticks(21, h.after(1, "readyz", 8318, "refused")); calls != "kill -s KILL clirelay2-8318\n" {
		t.Fatalf("a run that answered and then stopped listening must be SIGKILLed after 20 refusals, got %q", calls)
	}

	// the unit restarted between two rounds: the new run has to answer first
	h.reply("readyz", 8318, "204")
	restart := h.after(1, "readyz", 8318, "refused") + fmt.Sprintf(`; if [ "$i" = 1 ]; then echo %d > %q; fi`,
		time.Now().Add(-10*time.Minute).Unix(), filepath.Join(h.state, "clirelay2-8318.entered"))
	if calls := h.ticks(25, restart); calls != "" {
		t.Fatalf("an answer from the previous run must not count for the next one, got %q", calls)
	}
}

// The 2026-10-27 incident: after cutover to 8318 the drain stops 8319, whose
// listener is already closed; the old watchdog SIGKILLed it and started it
// again on the retired build.
func TestWatchdogLeavesTheRetiredSlotAlone(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", time.Hour)
	h.reply("readyz", 8318, "204")
	h.slot(8319, "active", "running", 2*time.Hour) // draining: listener closed
	h.reply("readyz", 8319, "refused")
	h.reply("healthz", 8319, "timeout")
	if calls := h.ticks(25, ""); calls != "" {
		t.Fatalf("a draining old slot must not be touched, got %q", calls)
	}
	if probes := h.probes(); strings.Contains(probes, "8319") {
		t.Fatalf("only the active slot may be probed, got %q", probes)
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
	for _, port := range []int{8318, 8319} {
		h.reply("readyz", port, "timeout")
		h.reply("healthz", port, "timeout")
	}
	// three silent rounds on 8319, then the deploy moves traffic to 8318
	moved := fmt.Sprintf(`if [ "$i" = 3 ]; then echo 8318 > %q; fi`, filepath.Join(h.base, ".active-port"))
	if calls := h.ticks(6, moved); calls != "" {
		t.Fatalf("silent rounds on the previous slot must not count against the new one, got %q", calls)
	}
	if !strings.Contains(h.output, "active slot changed 8319 -> 8318; now watching clirelay2-8318") {
		t.Fatalf("the switch must be logged, got:\n%s", h.output)
	}
}

func TestWatchdogWaitsOutStartupAndCrashes(t *testing.T) {
	h := newWatchdogHarness(t)
	h.activePort("8318")
	h.slot(8318, "active", "running", 10*time.Second)
	h.reply("readyz", 8318, "timeout")
	h.reply("healthz", 8318, "timeout")
	if calls := h.ticks(6, ""); calls != "" {
		t.Fatalf("a slot inside its startup grace must not be killed, got %q", calls)
	}
	h.slot(8318, "activating", "auto-restart", time.Hour)
	if calls := h.ticks(6, ""); calls != "" {
		t.Fatalf("a crashed slot belongs to Restart=always, got %q", calls)
	}
	for _, port := range []string{"garbage", ""} {
		h.activePort(port)
		if calls := h.ticks(6, ""); calls != "" {
			t.Fatalf("active-port %q must be ignored, got %q", port, calls)
		}
	}

	// curl itself failing says nothing about the slot
	h.activePort("8318")
	h.slot(8318, "active", "running", time.Hour)
	h.reply("readyz", 8318, "broken")
	if calls := h.ticks(10, ""); calls != "" {
		t.Fatalf("a probe that could not run must not count, got %q", calls)
	}
	if !strings.Contains(h.output, "clirelay2-8318: /readyz could not be probed (curl exit 2); not counted") {
		t.Fatalf("a probe that could not run must be logged, got:\n%s", h.output)
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
