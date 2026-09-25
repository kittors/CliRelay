package middleware

import (
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Quota decisions while the usage database cannot be read.
//
// Admitting a key with request or spending limits reads its usage from the
// database. During a database restart or primary failover those reads fail,
// and refusing with 503 turns a 30-60 second failover into an outage of the
// same length for every limited key. Each successful read is therefore kept
// per (window, subject), and when a read fails a kept reading no older than
// the configured maximum age decides instead. Past that age the original 503
// stands.
//
// The price is bounded overshoot: usage recorded while a kept reading stands
// in is not seen, so a key can run past a limit by at most what it spends in
// that window.

const (
	quotaWindowDayRequests   = "day_requests"
	quotaWindowTotalRequests = "total_requests"
	quotaWindowTotalCost     = "total_cost"
	quotaWindowDayCost       = "day_cost"
	quotaWindowPeriodKey     = "period_key"
	quotaWindowPeriodAccount = "period_account"

	defaultQuotaUsageStaleMaxAge = 120 * time.Second
	quotaUsageSweepEvery         = 1024
	quotaUsageStaleLogInterval   = 30 * time.Second
)

type quotaUsageCacheKey struct {
	window  string
	subject string
}

type quotaUsageReading struct {
	value  any
	readAt time.Time
	// day is the local date of the read. Windows that reset at midnight must
	// not answer for the next day with the previous day's total.
	day string
}

var (
	quotaUsageStaleMaxAge atomic.Int64

	quotaUsageCacheMu     sync.Mutex
	quotaUsageCache       = map[quotaUsageCacheKey]quotaUsageReading{}
	quotaUsageCacheWrites int

	quotaUsageStaleServed atomic.Int64
	quotaUsageStaleLogged sync.Map // log subject -> time.Time of the last warning
)

func init() {
	quotaUsageStaleMaxAge.Store(int64(defaultQuotaUsageStaleMaxAge))
}

// SetQuotaUsageStaleMaxAge sets how old a kept usage reading may be and still
// decide admission while the usage database cannot be read. Zero disables the
// fallback.
func SetQuotaUsageStaleMaxAge(maxAge time.Duration) {
	if maxAge < 0 {
		maxAge = 0
	}
	quotaUsageStaleMaxAge.Store(int64(maxAge))
}

// QuotaUsageStaleServed returns how many admission checks were decided on a
// kept reading because the usage database could not be read.
func QuotaUsageStaleServed() int64 { return quotaUsageStaleServed.Load() }

// readQuotaUsage runs read and keeps a successful result. When read fails it
// returns the kept result instead, if one exists that is recent enough and,
// for a dayBounded window, was read on the current local day.
func readQuotaUsage[T any](window, subject, logSubject string, dayBounded bool, read func() (T, error)) (T, error) {
	value, err := read()
	maxAge := time.Duration(quotaUsageStaleMaxAge.Load())
	if maxAge <= 0 {
		return value, err
	}
	now := time.Now()
	key := quotaUsageCacheKey{window: window, subject: subject}
	if err == nil {
		keepQuotaUsageReading(key, quotaUsageReading{value: value, readAt: now, day: now.Format(time.DateOnly)})
		return value, nil
	}
	quotaUsageCacheMu.Lock()
	reading, ok := quotaUsageCache[key]
	quotaUsageCacheMu.Unlock()
	if !ok {
		return value, err
	}
	age := now.Sub(reading.readAt)
	cached, typed := reading.value.(T)
	if !typed || age > maxAge || (dayBounded && reading.day != now.Format(time.DateOnly)) {
		return value, err
	}
	quotaUsageStaleServed.Add(1)
	warnStaleQuotaUsage(window, logSubject, age, err, now)
	return cached, nil
}

func keepQuotaUsageReading(key quotaUsageCacheKey, reading quotaUsageReading) {
	quotaUsageCacheMu.Lock()
	defer quotaUsageCacheMu.Unlock()
	quotaUsageCache[key] = reading
	quotaUsageCacheWrites++
	if quotaUsageCacheWrites%quotaUsageSweepEvery != 0 {
		return
	}
	// Readings past the maximum age can never be served again.
	cutoff := reading.readAt.Add(-time.Duration(quotaUsageStaleMaxAge.Load()))
	for k, r := range quotaUsageCache {
		if r.readAt.Before(cutoff) {
			delete(quotaUsageCache, k)
		}
	}
}

func warnStaleQuotaUsage(window, logSubject string, age time.Duration, err error, now time.Time) {
	if last, ok := quotaUsageStaleLogged.Load(logSubject); ok && now.Sub(last.(time.Time)) < quotaUsageStaleLogInterval {
		return
	}
	quotaUsageStaleLogged.Store(logSubject, now)
	log.WithError(err).Warnf("quota: usage database unavailable; admitting %s on its %s usage read %s ago (%d decisions on kept readings so far)",
		logSubject, window, age.Round(time.Second), quotaUsageStaleServed.Load())
}

// resetQuotaUsageFallback clears kept readings; tests use it for isolation.
func resetQuotaUsageFallback() {
	quotaUsageCacheMu.Lock()
	quotaUsageCache = map[quotaUsageCacheKey]quotaUsageReading{}
	quotaUsageCacheWrites = 0
	quotaUsageCacheMu.Unlock()
	quotaUsageStaleLogged = sync.Map{}
}

// usageSubjects names the policy's usage pool for the reading cache and, with
// a stable identifier instead of the secret, for logs.
func (p *quotaPolicy) usageSubjects() (subject, logSubject string) {
	if p.endUserID != "" {
		return "eu:" + p.endUserID, "account " + p.endUserID
	}
	if p.apiKeyID != "" {
		return "key:" + p.apiKey, "key " + p.apiKeyID
	}
	return "key:" + p.apiKey, "key " + maskKey(p.apiKey)
}
