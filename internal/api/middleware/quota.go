package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// ─── Sliding window counters ────────────────────────────────────────────────

const windowDuration = 60 * time.Second

// slidingWindow tracks timestamped events within the last 60 seconds.
type slidingWindow struct {
	mu     sync.Mutex
	events []time.Time
}

func (w *slidingWindow) add() {
	now := time.Now()
	w.mu.Lock()
	w.events = append(w.events, now)
	w.mu.Unlock()
}

func (w *slidingWindow) count() int {
	cutoff := time.Now().Add(-windowDuration)
	w.mu.Lock()
	defer w.mu.Unlock()
	// Trim old events
	i := 0
	for i < len(w.events) && w.events[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		w.events = w.events[i:]
	}
	return len(w.events)
}

// tokenWindow tracks timestamped token counts within the last 60 seconds.
type tokenWindow struct {
	mu      sync.Mutex
	entries []tokenEntry
	total   atomic.Int64 // cached total for fast reads
}

type tokenEntry struct {
	ts     time.Time
	tokens int64
}

func (w *tokenWindow) add(tokens int64) {
	now := time.Now()
	w.mu.Lock()
	w.entries = append(w.entries, tokenEntry{ts: now, tokens: tokens})
	w.mu.Unlock()
	w.total.Add(tokens)
}

func (w *tokenWindow) sum() int64 {
	cutoff := time.Now().Add(-windowDuration)
	w.mu.Lock()
	defer w.mu.Unlock()
	// Trim old entries and recalculate
	i := 0
	var expired int64
	for i < len(w.entries) && w.entries[i].ts.Before(cutoff) {
		expired += w.entries[i].tokens
		i++
	}
	if i > 0 {
		w.entries = w.entries[i:]
		w.total.Add(-expired)
	}
	return w.total.Load()
}

// ─── Per-key tracker registry ───────────────────────────────────────────────

var (
	rpmTrackers sync.Map // map[string]*slidingWindow
	tpmTrackers sync.Map // map[string]*tokenWindow
)

func getRPMTracker(apiKey string) *slidingWindow {
	if v, ok := rpmTrackers.Load(apiKey); ok {
		return v.(*slidingWindow)
	}
	w := &slidingWindow{}
	actual, _ := rpmTrackers.LoadOrStore(apiKey, w)
	return actual.(*slidingWindow)
}

func getTPMTracker(apiKey string) *tokenWindow {
	if v, ok := tpmTrackers.Load(apiKey); ok {
		return v.(*tokenWindow)
	}
	w := &tokenWindow{}
	actual, _ := tpmTrackers.LoadOrStore(apiKey, w)
	return actual.(*tokenWindow)
}

// RecordTokenUsage records token consumption for TPM tracking.
// This should be called by the usage reporter after a request completes.
func RecordTokenUsage(apiKey string, totalTokens int64) {
	if apiKey == "" || totalTokens <= 0 {
		return
	}
	getTPMTracker(apiKey).add(totalTokens)
}

// ─── Quota Middleware ───────────────────────────────────────────────────────

// QuotaMiddleware enforces daily-limit, total-quota, RPM (requests per minute),
// and TPM (tokens per minute) restrictions for authenticated API keys.
//
// It reads the limits from the accessMetadata set by the auth provider.
// This middleware MUST be placed after AuthMiddleware and before route handlers.
// Only POST requests are checked (GET /models etc. don't consume quota).
func QuotaMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only enforce on POST requests (actual API calls)
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Get the authenticated API key
		apiKeyVal, exists := c.Get("apiKey")
		if !exists {
			c.Next()
			return
		}
		apiKey, ok := apiKeyVal.(string)
		if !ok || apiKey == "" {
			c.Next()
			return
		}

		// ── Always record this request for system-wide RPM tracking ──
		// This must happen before any metadata checks so ALL authenticated
		// POST requests are counted for the dashboard RPM display.
		rpmTracker := getRPMTracker(apiKey)
		rpmTracker.add()

		// Get access metadata containing limits
		metadataVal, exists := c.Get("accessMetadata")
		if !exists {
			c.Next()
			return
		}
		metadata, ok := metadataVal.(map[string]string)
		if !ok {
			c.Next()
			return
		}

		// Parse limits from metadata
		dailyLimit := parseIntMetadata(metadata, "daily-limit")
		totalQuota := parseIntMetadata(metadata, "total-quota")
		rpmLimit := parseIntMetadata(metadata, "rpm-limit")
		tpmLimit := parseIntMetadata(metadata, "tpm-limit")
		spendingLimit := parseFloatMetadata(metadata, "spending-limit")

		// Cache limits for dashboard snapshot
		UpdateKeyLimits(apiKey, rpmLimit, tpmLimit)

		// No limits configured — skip all checks
		if dailyLimit <= 0 && totalQuota <= 0 && rpmLimit <= 0 && tpmLimit <= 0 && spendingLimit <= 0 {
			c.Next()
			return
		}

		// --- RPM check (sliding window, in-memory) ---
		if rpmLimit > 0 {
			if rpmTracker.count() > rpmLimit {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("RPM limit (%d requests/min) exceeded for this API key", rpmLimit),
						"type":    "rate_limit_exceeded",
						"code":    "rpm_limit_exceeded",
					},
				})
				return
			}
		}

		// --- TPM check (sliding window, in-memory) ---
		if tpmLimit > 0 {
			tracker := getTPMTracker(apiKey)
			if tracker.sum() >= int64(tpmLimit) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("TPM limit (%d tokens/min) exceeded for this API key", tpmLimit),
						"type":    "rate_limit_exceeded",
						"code":    "tpm_limit_exceeded",
					},
				})
				return
			}
		}

		// --- Daily limit check (from usage DB) ---
		if dailyLimit > 0 {
			todayCount, err := countTodayByKeyFunc(apiKey)
			if err != nil {
				log.Warnf("quota: failed to query daily usage for key %s: %v", maskKey(apiKey), err)
			} else if todayCount >= int64(dailyLimit) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("daily request limit (%d) exceeded for this API key", dailyLimit),
						"type":    "rate_limit_exceeded",
						"code":    "daily_limit_exceeded",
					},
				})
				return
			}
		}

		// --- Total quota check (from usage DB) ---
		if totalQuota > 0 {
			totalCount, err := countTotalByKeyFunc(apiKey)
			if err != nil {
				log.Warnf("quota: failed to query total usage for key %s: %v", maskKey(apiKey), err)
			} else if totalCount >= int64(totalQuota) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("total request quota (%d) exhausted for this API key", totalQuota),
						"type":    "rate_limit_exceeded",
						"code":    "total_quota_exceeded",
					},
				})
				return
			}
		}

		// --- Spending limit check (from usage DB) ---
		if spendingLimit > 0 {
			totalCost, err := queryTotalCostByKeyFunc(apiKey)
			if err != nil {
				log.Warnf("quota: failed to query total cost for key %s: %v", maskKey(apiKey), err)
			} else if totalCost >= spendingLimit {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("spending limit ($%.2f) exceeded for this API key (current: $%.2f)", spendingLimit, totalCost),
						"type":    "rate_limit_exceeded",
						"code":    "spending_limit_exceeded",
					},
				})
				return
			}
		}

		c.Next()
	}
}

// ─── Usage DB query functions (injected to avoid import cycle) ──────────────

// countTodayByKeyFunc and countTotalByKeyFunc are set by InitQuotaUsageFuncs.
// They default to no-ops that always return 0 (no limit enforced) until set.
var (
	countTodayByKeyFunc     = func(string) (int64, error) { return 0, nil }
	countTotalByKeyFunc     = func(string) (int64, error) { return 0, nil }
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 0, nil }
)

// InitQuotaUsageFuncs injects the usage DB query functions into the middleware.
// This avoids a direct import of the usage package which would cause cycles.
func InitQuotaUsageFuncs(
	countToday func(string) (int64, error),
	countTotal func(string) (int64, error),
	totalCost func(string) (float64, error),
) {
	countTodayByKeyFunc = countToday
	countTotalByKeyFunc = countTotal
	queryTotalCostByKeyFunc = totalCost
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func parseIntMetadata(metadata map[string]string, key string) int {
	v, ok := metadata[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func parseFloatMetadata(metadata map[string]string, key string) float64 {
	v, ok := metadata[key]
	if !ok {
		return 0
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return n
}

// ─── Dashboard snapshot (for system_stats) ──────────────────────────────────

// ConcurrencySnapshot represents real-time rate info for a single API key.
type ConcurrencySnapshot struct {
	APIKey   string `json:"api_key"`
	RPM      int    `json:"rpm"`       // current requests in the last 60s
	TPM      int64  `json:"tpm"`       // current tokens in the last 60s
	RPMLimit int    `json:"rpm_limit"` // configured limit (0 = unlimited)
	TPMLimit int    `json:"tpm_limit"` // configured limit (0 = unlimited)
}

// snapshotLimits stores the configured limits per key for dashboard display.
var snapshotLimits sync.Map // map[string][2]int  {rpmLimit, tpmLimit}

// UpdateKeyLimits stores the configured RPM/TPM limits for a key so the
// dashboard snapshot can display them. Called during auth.
func UpdateKeyLimits(apiKey string, rpmLimit, tpmLimit int) {
	if apiKey == "" {
		return
	}
	snapshotLimits.Store(apiKey, [2]int{rpmLimit, tpmLimit})
}

// GetConcurrencySnapshot returns a list of API keys with active RPM/TPM usage
// and the total in-flight request count (sum of all RPM counters).
func GetConcurrencySnapshot() ([]ConcurrencySnapshot, int64) {
	var snapshots []ConcurrencySnapshot
	var totalInFlight int64

	rpmTrackers.Range(func(key, value any) bool {
		apiKey := key.(string)
		w := value.(*slidingWindow)
		rpm := w.count()

		var tpm int64
		if tv, ok := tpmTrackers.Load(apiKey); ok {
			tpm = tv.(*tokenWindow).sum()
		}

		if rpm > 0 || tpm > 0 {
			snap := ConcurrencySnapshot{
				APIKey: apiKey,
				RPM:    rpm,
				TPM:    tpm,
			}
			if limits, ok := snapshotLimits.Load(apiKey); ok {
				l := limits.([2]int)
				snap.RPMLimit = l[0]
				snap.TPMLimit = l[1]
			}
			snapshots = append(snapshots, snap)
			totalInFlight += int64(rpm)
		}
		return true
	})

	return snapshots, totalInFlight
}
