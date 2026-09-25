package config

import (
	"crypto/sha256"
	"path/filepath"
	"sync"
	"time"
)

// selfWriteTTL bounds how long a write this process made to config.yaml is
// remembered. The watcher debounces for a fraction of a second, so anything
// older is no longer the change it is looking at.
const selfWriteTTL = 30 * time.Second

var selfWrites struct {
	sync.Mutex
	byPath map[string]map[[sha256.Size]byte]time.Time
}

func selfWriteKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// noteSelfWrite records that this process wrote data to path. Every writer of
// config.yaml in this process (management saves, the database-section cleanup,
// the provider-ID migration) has already applied what it wrote to the running
// configuration before or right after writing it.
func noteSelfWrite(path string, data []byte) {
	sum := sha256.Sum256(data)
	key := selfWriteKey(path)
	now := time.Now()
	selfWrites.Lock()
	defer selfWrites.Unlock()
	if selfWrites.byPath == nil {
		selfWrites.byPath = make(map[string]map[[sha256.Size]byte]time.Time)
	}
	sums := selfWrites.byPath[key]
	if sums == nil {
		sums = make(map[[sha256.Size]byte]time.Time)
		selfWrites.byPath[key] = sums
	}
	for old, at := range sums {
		if now.Sub(at) > selfWriteTTL {
			delete(sums, old)
		}
	}
	sums[sum] = now
}

// WrittenByThisProcess reports whether content with this SHA-256 is what this
// process recently wrote to path. The config watcher uses it to skip reloading
// a file change that is the echo of a save the process already applied: that
// second full reload rebuilt every executor and cut upstream sessions for
// nothing.
func WrittenByThisProcess(path string, sum [sha256.Size]byte) bool {
	selfWrites.Lock()
	defer selfWrites.Unlock()
	at, ok := selfWrites.byPath[selfWriteKey(path)][sum]
	return ok && time.Since(at) <= selfWriteTTL
}

// ForgetSelfWrites drops what was recorded for path. The watcher calls it
// after reloading an external change, so a later revert to content this
// process once wrote is reloaded like any other edit.
func ForgetSelfWrites(path string) {
	selfWrites.Lock()
	delete(selfWrites.byPath, selfWriteKey(path))
	selfWrites.Unlock()
}
