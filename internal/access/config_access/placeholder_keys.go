package configaccess

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// placeholderRejectWarnInterval spaces out the warning for one example key: a
// client that keeps retrying with it would otherwise add a log line per request.
const placeholderRejectWarnInterval = 5 * time.Minute

// rejectWarnLimiter lets one warning per key through each interval.
type rejectWarnLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
}

func (l *rejectWarnLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[key]; ok && now.Sub(last) < l.interval {
		return false
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	l.last[key] = now
	return true
}

// placeholderRejectWarnings is process-wide rather than per provider, because the
// provider is rebuilt on every key change and config reload. Only configured
// example keys reach it, so it holds at most one entry per example key.
var placeholderRejectWarnings = &rejectWarnLimiter{interval: placeholderRejectWarnInterval}

// warnPlaceholderKeyRejected tells the operator that a configured key was refused
// because it is one of the example keys. The value is logged as-is: it is
// published with the repository, and it is what the operator has to look for.
func warnPlaceholderKeyRejected(key, name string) {
	if !placeholderRejectWarnings.allow(key, time.Now()) {
		return
	}
	label := key
	if name != "" {
		label = name + " (" + key + ")"
	}
	log.Warnf("rejected client API key %s: it is an example key published in config.example.yaml and cannot authenticate; create a real key on the API Keys page of the management panel and switch this client to it", label)
}
