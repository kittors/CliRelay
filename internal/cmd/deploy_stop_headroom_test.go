package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A deploy SIGTERMs the retiring slot, which drains HTTP for its shutdown grace
// (CLIRELAY_SHUTDOWN_GRACE). The rest of the shutdown runs after the drain and
// none of it shares the grace's deadline, so systemd waits
// SHUTDOWN_STOP_HEADROOM_SECONDS past the grace before SIGKILL
// (scripts/deploy-blue-green.sh). When the drain uses the whole grace, these are
// the steps that headroom has to cover, one after the other. Raise the headroom
// along with any of them, and sync the root-managed script to every node.
func TestDeployStopHeadroomCoversTheShutdownAfterTheDrain(t *testing.T) {
	const (
		// The spool replayer finishes the write it is in the middle of before it
		// stops: one attempt, requestLogWriteAttemptTimeout in internal/usage.
		spoolReplayWrite = 10 * time.Second
		// StopRedis waits out a periodic snapshot save that may be under way,
		// then saves once more. With go-redis defaults a save against a server
		// that stopped answering is four tries of a 3s read timeout.
		redisSnapshotSaves = 2 * 4 * 3 * time.Second
		// CloseDB waits for the request log maintenance, which has to stop
		// within one statement of the cancel (kittors/CliRelay#1266); the
		// statements it runs without a ctx are short.
		maintenanceStatement = 5 * time.Second
	)
	// This package's own bounds: closing the cluster credential store (cluster
	// mode only) and waiting for the queued usage records.
	need := clusterAuthCloseTimeout + usageSpoolStopTimeout + spoolReplayWrite + redisSnapshotSaves + maintenanceStatement

	if headroom := deployScriptSeconds(t, "SHUTDOWN_STOP_HEADROOM_SECONDS"); headroom < need {
		t.Fatalf("SHUTDOWN_STOP_HEADROOM_SECONDS is %s, but the shutdown after the drain may take %s; systemd would SIGKILL it first", headroom, need)
	}
}

// deployScriptSeconds reads a NAME=<seconds> setting from the deploy script.
func deployScriptSeconds(t *testing.T, name string) time.Duration {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "deploy-blue-green.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), name+"=")
		if !ok {
			continue
		}
		seconds, err := strconv.Atoi(strings.Trim(value, `'"`))
		if err != nil {
			t.Fatalf("%s in %s is not a number of seconds: %q", name, path, value)
		}
		return time.Duration(seconds) * time.Second
	}
	t.Fatalf("%s does not set %s", path, name)
	return 0
}
