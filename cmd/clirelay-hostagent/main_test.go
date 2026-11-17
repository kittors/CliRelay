package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/nodemetrics"
)

// The unit runs on the database host. On Ubuntu 22.04 its systemd 249 does
// not know the %d specifier and refuses to load a unit that uses one.
func TestSystemdUnitAvoidsTheCredentialsSpecifier(t *testing.T) {
	data, err := os.ReadFile("../../deploy/cluster/hostagent/clirelay-hostagent.service")
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	unit := string(data)
	if !strings.Contains(unit, "-dsn-file ${CREDENTIALS_DIRECTORY}/dsn") {
		t.Errorf("ExecStart must read the DSN from ${CREDENTIALS_DIRECTORY}")
	}
	for _, line := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "%d") {
			t.Errorf("unit uses the %%d specifier, which systemd 249 cannot resolve: %q", line)
		}
	}
}

func TestPathFlags(t *testing.T) {
	var paths pathFlags
	for _, value := range []string{"database=/opt/clirelay-data/pg", "/opt/clirelay-cluster/backups", " backups = /srv/wal "} {
		if err := paths.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}
	want := []nodemetrics.DiskPath{
		{Use: "database", Path: "/opt/clirelay-data/pg"},
		{Use: nodemetrics.DiskUseExtra, Path: "/opt/clirelay-cluster/backups"},
		{Use: "backups", Path: "/srv/wal"},
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %+v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %+v, want %+v", i, paths[i], want[i])
		}
	}
	for _, bad := range []string{"", "=", "logs=", "=/x", "relative/dir", "use=relative"} {
		if err := paths.Set(bad); err == nil {
			t.Fatalf("Set(%q) accepted", bad)
		}
	}
}

func TestFailureHintNamesTheUsualCauses(t *testing.T) {
	if hint := failureHint(errors.New(`ERROR: relation "cluster_hosts" does not exist (SQLSTATE 42P01)`)); hint == "" {
		t.Fatal("no hint for a missing table")
	}
	if hint := failureHint(errors.New(`ERROR: permission denied for table cluster_hosts (SQLSTATE 42501)`)); hint == "" {
		t.Fatal("no hint for a missing grant")
	}
	if hint := failureHint(errors.New("dial tcp 127.0.0.1:25432: connect: connection refused")); hint != "" {
		t.Fatalf("hint for a connection error = %q", hint)
	}
}
