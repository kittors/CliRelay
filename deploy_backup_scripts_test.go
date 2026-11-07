package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The backup scripts in deploy/cluster/backup serve two layouts: the cluster
// arbiter that reaches the members over TLS, and a single node that backs up
// its own database container through the container's loopback. These tests
// run the shipped scripts with a fake docker (and rsync) that records what it
// was asked to do.

const backupDir = "deploy/cluster/backup"

// backupFakeDocker records each call in $FAKE_DOCKER_LOG and plays the parts
// the scripts read back: the slot cap psql reports, and the finished archive
// pg-basebackup.sh measures and prunes by (its backup_label names
// $FAKE_START_WAL as the first segment).
const backupFakeDocker = `#!/usr/bin/env bash
printf '%s\n' "$(printf '%s' "$*" | tr '\n' ' ')" >> "$FAKE_DOCKER_LOG"
case "$*" in
  *"show max_slot_wal_keep_size"*) echo "${FAKE_SLOT_CAP:-4GB}"; exit 0 ;;
esac
if [[ "$*" == *"mv /backups/.tmp-"* ]]; then
  host=""
  for a in "$@"; do case "$a" in *:/backups) host="${a%:/backups}" ;; esac; done
  name="$(printf '%s' "$*" | sed -n 's#.*mv /backups/\.tmp-[^ ]* /backups/\(base-[0-9TZ]*\)\.tar\.gz.*#\1#p')"
  tmp="$(mktemp -d)"
  printf 'START WAL LOCATION: 0/5000028 (file %s)\n' "${FAKE_START_WAL:-000000010000000000000009}" > "$tmp/backup_label"
  tar -C "$tmp" -czf "$host/$name.tar.gz" ./backup_label
fi
exit 0
`

type backupHarness struct {
	t      *testing.T
	dir    string
	bin    string
	backup string
	log    string
	env    string
}

func newBackupHarness(t *testing.T, config string) *backupHarness {
	t.Helper()
	dir := t.TempDir()
	h := &backupHarness{t: t, dir: dir, bin: filepath.Join(dir, "bin"), backup: filepath.Join(dir, "backups"),
		log: filepath.Join(dir, "docker.log"), env: filepath.Join(dir, "backup.env")}
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(h.bin, "docker"), backupFakeDocker)
	if err := os.WriteFile(h.env, []byte(config+"\nBACKUP_DIR="+h.backup+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// archive writes a base backup whose backup_label starts at startWAL, aged
// so that "ls -t" orders it before anything newer.
func (h *backupHarness) archive(name, startWAL string, age time.Duration) {
	h.t.Helper()
	if err := os.MkdirAll(h.backup, 0o700); err != nil {
		h.t.Fatal(err)
	}
	tmp := h.t.TempDir()
	label := fmt.Sprintf("START WAL LOCATION: 0/3000028 (file %s)\n", startWAL)
	if err := os.WriteFile(filepath.Join(tmp, "backup_label"), []byte(label), 0o600); err != nil {
		h.t.Fatal(err)
	}
	path := filepath.Join(h.backup, name)
	if out, err := exec.Command("tar", "-C", tmp, "-czf", path, "./backup_label").CombinedOutput(); err != nil {
		h.t.Fatalf("tar: %v\n%s", err, out)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		h.t.Fatal(err)
	}
}

func (h *backupHarness) run(script string, env ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command("bash", filepath.Join(backupDir, script))
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + h.bin + ":" + os.Getenv("PATH"),
		"BACKUP_ENV=" + h.env, "FAKE_DOCKER_LOG=" + h.log}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// calls returns the docker invocations containing marker.
func (h *backupHarness) calls(marker string) []string {
	data, _ := os.ReadFile(h.log)
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, marker) {
			found = append(found, line)
		}
	}
	return found
}

func requireAll(t *testing.T, what, line string, want []string, unwanted []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Fatalf("%s must contain %q:\n%s", what, w, line)
		}
	}
	for _, u := range unwanted {
		if strings.Contains(line, u) {
			t.Fatalf("%s must not contain %q:\n%s", what, u, line)
		}
	}
}

func TestBaseBackupOnASingleNodeUsesTheContainerLoopback(t *testing.T) {
	h := newBackupHarness(t, "PG_CONTAINER=clirelay-pg\nPG_SUPERUSER=cliproxy\nKEEP=2")
	h.archive("base-20260101T000000Z.tar.gz", "000000010000000000000003", 48*time.Hour)
	h.archive("base-20260102T000000Z.tar.gz", "000000010000000000000006", 24*time.Hour)
	if err := os.MkdirAll(filepath.Join(h.backup, "wal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := h.run("pg-basebackup.sh"); err != nil {
		t.Fatalf("pg-basebackup.sh: %v\n%s", err, out)
	}
	runs := h.calls("pg_basebackup")
	if len(runs) != 1 {
		t.Fatalf("want one pg_basebackup run, got %d", len(runs))
	}
	// The database port is published on 127.0.0.1 only; the backup joins the
	// container's network namespace instead of opening anything, and as it
	// shares the host with live traffic it takes one core and a spread
	// checkpoint.
	requireAll(t, "the single-node backup", runs[0],
		[]string{"--network container:clirelay-pg", "--cpus=1", "host=127.0.0.1 port=5432 user=cliproxy", "-c spread", "--compress=none", "pg_verifybackup"},
		[]string{"/tls", "sslmode", "prefer-standby"})

	// KEEP=2: the new backup and the newer old one stay, and WAL is pruned to
	// what the oldest kept backup needs.
	left, _ := filepath.Glob(filepath.Join(h.backup, "base-*.tar.gz"))
	if len(left) != 2 || strings.Contains(strings.Join(left, " "), "base-20260101T000000Z") {
		t.Fatalf("retention kept the wrong backups: %v", left)
	}
	if cleanup := h.calls("pg_archivecleanup"); len(cleanup) != 1 || !strings.HasSuffix(cleanup[0], " 000000010000000000000006") {
		t.Fatalf("WAL must be pruned before the oldest kept backup's start, got %v", cleanup)
	}
}

func TestBaseBackupOnTheClusterArbiterIsUnchanged(t *testing.T) {
	h := newBackupHarness(t, "BACKUP_HOSTS=198.51.100.20,198.51.100.21\nBACKUP_PORT=55432\nPGPASSWORD=secret")
	if out, err := h.run("pg-basebackup.sh"); err != nil {
		t.Fatalf("pg-basebackup.sh: %v\n%s", err, out)
	}
	runs := h.calls("pg_basebackup")
	if len(runs) != 1 {
		t.Fatalf("want one pg_basebackup run, got %d", len(runs))
	}
	requireAll(t, "the cluster backup", runs[0],
		[]string{"--network host", "-v /etc/clirelay-cluster/tls:/tls:ro", "user=replicator", "port=55432,55432", "target_session_attrs=prefer-standby", "-c fast", "--compress=server-zstd"},
		[]string{"--cpus", "container:"})
}

func TestBaseBackupNeedsALayout(t *testing.T) {
	h := newBackupHarness(t, "KEEP=7")
	out, err := h.run("pg-basebackup.sh")
	if err == nil || !strings.Contains(out, "PG_CONTAINER") {
		t.Fatalf("a config naming neither layout must fail clearly, err=%v\n%s", err, out)
	}
}

// A replication slot keeps WAL on the server while the archiver is down. On a
// single node nothing else caps it, so an archiver that stays stopped would
// fill the production disk.
func TestReceiveWALRefusesAnUncappedSlotOnASingleNode(t *testing.T) {
	h := newBackupHarness(t, "PG_CONTAINER=clirelay-pg")
	out, err := h.run("pg-receivewal.sh", "FAKE_SLOT_CAP=-1")
	if err == nil || !strings.Contains(out, "max_slot_wal_keep_size") {
		t.Fatalf("an unlimited slot cap must stop the archiver, err=%v\n%s", err, out)
	}
	if len(h.calls("--create-slot")) != 0 || len(h.calls("pg_receivewal -D")) != 0 {
		t.Fatalf("no slot may be created without a cap")
	}

	h = newBackupHarness(t, "PG_CONTAINER=clirelay-pg")
	if out, err := h.run("pg-receivewal.sh", "FAKE_SLOT_CAP=4GB"); err != nil {
		t.Fatalf("pg-receivewal.sh: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(h.log)
	create := strings.Index(string(data), "pg_receivewal --create-slot --if-not-exists --slot=clirelay_walarchive")
	stream := strings.Index(string(data), "pg_receivewal -D /wal --slot=clirelay_walarchive --compress=gzip")
	if create < 0 || stream < create || !strings.Contains(string(data), "--network container:clirelay-pg") {
		t.Fatalf("the archiver must create its slot, then stream through the container loopback:\n%s", data)
	}
}

func TestReceiveWALOnTheClusterLeavesSlotsToPatroni(t *testing.T) {
	// The unit loads backup.env through systemd's EnvironmentFile=, which
	// keeps an unquoted value with spaces whole; sourcing the same line in
	// bash would cut the DSN at the first space.
	const dsn = "host=198.51.100.20,198.51.100.21 port=55432,55432 user=replicator"
	h := newBackupHarness(t, "BACKUP_HOSTS=198.51.100.20\nWAL_ARCHIVE_DSN="+dsn)
	if out, err := h.run("pg-receivewal.sh", "WAL_ARCHIVE_DSN="+dsn); err != nil {
		t.Fatalf("pg-receivewal.sh: %v\n%s", err, out)
	}
	if len(h.calls("--create-slot")) != 0 || len(h.calls("max_slot_wal_keep_size")) != 0 {
		t.Fatalf("Patroni owns the cluster's slot")
	}
	if s := h.calls("pg_receivewal -D /wal"); len(s) != 1 || !strings.Contains(s[0], "--dbname="+dsn) {
		t.Fatalf("the cluster archiver must follow WAL_ARCHIVE_DSN as systemd passed it, got %v", s)
	}
	unit, err := os.ReadFile(filepath.Join(backupDir, "clirelay-pg-receivewal.service"))
	if err != nil || !strings.Contains(string(unit), "EnvironmentFile=/etc/clirelay-cluster/backup.env") {
		t.Fatalf("the unit must keep loading backup.env through systemd: %v", err)
	}
}

// The offsite host pulls and the database host holds no credential for it,
// so a compromised database host cannot reach the copies. rsync must not
// mirror deletions either: the offsite host prunes its own copies.
func TestOffsitePullNeverDeletesAndDropsFinishedPartials(t *testing.T) {
	h := newBackupHarness(t, "PULL_SOURCE=root@203.0.113.10\nPULL_PORT=2222\nKEEP=2")
	wal := filepath.Join(h.backup, "wal")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	// left from an earlier pull: the segment has since been completed
	writeFile := func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(wal, "000000010000000000000007.gz.partial"))
	h.archive("base-20260101T000000Z.tar.gz", "000000010000000000000003", 72*time.Hour)
	writeExecutable(t, filepath.Join(h.bin, "rsync"), `#!/usr/bin/env bash
printf '%s\n' "$@" > "$FAKE_RSYNC_ARGS"
dest="${@: -1}"
mkdir -p "$dest/wal"
for f in 000000010000000000000007.gz 000000010000000000000008.gz.partial; do printf x > "$dest/wal/$f"; done
`)
	rsyncArgs := filepath.Join(h.dir, "rsync.args")
	h.archive("base-20260102T000000Z.tar.gz", "000000010000000000000005", 48*time.Hour)
	h.archive("base-20260103T000000Z.tar.gz", "000000010000000000000007", 24*time.Hour)
	if out, err := h.run("offsite-pull.sh", "FAKE_RSYNC_ARGS="+rsyncArgs); err != nil {
		t.Fatalf("offsite-pull.sh: %v\n%s", err, out)
	}
	args, _ := os.ReadFile(rsyncArgs)
	requireAll(t, "the rsync call", string(args),
		[]string{"--exclude=.tmp-*", "-p 2222", "StrictHostKeyChecking=yes", "UserKnownHostsFile=", "root@203.0.113.10:/"},
		[]string{"--delete", "--remove-source-files"})
	if _, err := os.Stat(filepath.Join(wal, "000000010000000000000007.gz.partial")); !os.IsNotExist(err) {
		t.Fatalf("a .partial whose finished segment arrived must be dropped")
	}
	if _, err := os.Stat(filepath.Join(wal, "000000010000000000000008.gz.partial")); err != nil {
		t.Fatalf("the segment still being written must be kept: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(h.backup, "base-*.tar.gz"))
	if len(left) != 2 || strings.Contains(strings.Join(left, " "), "base-20260101T000000Z") {
		t.Fatalf("the offsite host must prune its own copies to KEEP, got %v", left)
	}
}

// Recovery refuses settings below the primary's, and hk-155 starts its
// database with "-c max_connections=200" while the postgresql.conf inside the
// backup keeps the default 100: the first offsite drill failed on exactly that.
func TestDrillStartsWithTheSettingsInTheControlFile(t *testing.T) {
	fn := extractShellFunction(t, filepath.Join(backupDir, "pitr-drill.sh"), "control_file_params")
	controldata := strings.Join([]string{
		"pg_control version number:            1300",
		"wal_level setting:                    replica",
		"max_connections setting:              200",
		"max_worker_processes setting:         8",
		"max_wal_senders setting:              10",
		"max_prepared_xacts setting:           0",
		"max_locks_per_xact setting:           64",
		"track_commit_timestamp setting:       off",
	}, "\n")
	cmd := exec.Command("bash", "-c", fn+"\ncontrol_file_params")
	cmd.Stdin = strings.NewReader(controldata + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("control_file_params: %v\n%s", err, out)
	}
	want := "-c\nmax_connections=200\n-c\nmax_worker_processes=8\n-c\nmax_wal_senders=10\n-c\nmax_prepared_transactions=0\n-c\nmax_locks_per_transaction=64\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestDrillCopiesOnlyTheWALTheBackupCanReplay(t *testing.T) {
	fn := extractShellFunction(t, filepath.Join(backupDir, "pitr-drill.sh"), "copy_needed_wal")
	src, dst := t.TempDir(), t.TempDir()
	for _, f := range []string{"00000002.history.gz", "000000010000000000000003.gz", "000000010000000000000005.gz",
		"000000020000000000000006.gz.partial"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copied := func(start string) string {
		t.Helper()
		entries, _ := os.ReadDir(dst)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(dst, e.Name()))
		}
		cmd := exec.Command("bash", "-c", fn+"\ncopy_needed_wal \"$1\" \"$2\" \"$3\"", "", src, dst, start)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("copy_needed_wal: %v\n%s", err, out)
		}
		entries, _ = os.ReadDir(dst)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return strings.Join(names, " ")
	}
	if got := copied("000000010000000000000005"); got != "000000010000000000000005.gz 00000002.history.gz 000000020000000000000006.gz.partial" {
		t.Fatalf("from segment 5: got %q", got)
	}
	if got := copied(""); !strings.Contains(got, "000000010000000000000003.gz") {
		t.Fatalf("an unknown start must copy everything, got %q", got)
	}
}
