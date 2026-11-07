package contentmoderation

import (
	"sync"
	"time"
)

// logLimiter spaces out log lines that one tenant could otherwise write on
// every request: a client that keeps sending a listed word makes a line per
// request for as long as it likes. The request log still records every hit; the
// process log only has to tell an operator that it is happening.
//
// A line that is let through says how many it stands for (see allow).
type logLimiter struct {
	mu      sync.Mutex
	entries map[string]logEntry
}

type logEntry struct {
	at time.Time
	// dropped counts the lines refused since the last one that went out.
	dropped int
}

// maxLimitedLogKeys bounds the keys remembered. A key is a tenant and a rule,
// so the bound is reached only by an unusual number of distinct ones; the
// limiter then stops letting new keys through, rather than grow, until the old
// ones have aged out.
const maxLimitedLogKeys = 4096

// allow reports whether a line for key may be written now, and how many lines
// for the same key were dropped since the last one that was. A key gets at most
// one line in every interval.
func (l *logLimiter) allow(key string, every time.Duration, now time.Time) (ok bool, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, seen := l.entries[key]
	if seen && now.Sub(entry.at) < every {
		entry.dropped++
		l.entries[key] = entry
		return false, 0
	}
	if l.entries == nil {
		l.entries = make(map[string]logEntry)
	}
	if !seen && len(l.entries) >= maxLimitedLogKeys {
		for old, e := range l.entries {
			if now.Sub(e.at) >= every {
				delete(l.entries, old)
			}
		}
		if len(l.entries) >= maxLimitedLogKeys {
			return false, 0
		}
	}
	l.entries[key] = logEntry{at: now}
	return true, entry.dropped
}
