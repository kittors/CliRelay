package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
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

// ─── Per-subject tracker registry ───────────────────────────────────────────
// Tracker keys are end-user ids when present, else the API key secret.
// Owned keys therefore share one RPM/TPM/concurrency pool per account.

var (
	rpmTrackers sync.Map // map[string]*slidingWindow
	tpmTrackers sync.Map // map[string]*tokenWindow

	inFlightMu    sync.Mutex
	inFlightByKey = map[string]int{}
)

func quotaSubjectKey(apiKey string, metadata map[string]string) string {
	if metadata != nil {
		if id := strings.TrimSpace(metadata["end-user-id"]); id != "" {
			return "eu:" + id
		}
	}
	return apiKey
}

func getRPMTracker(subject string) *slidingWindow {
	if v, ok := rpmTrackers.Load(subject); ok {
		return v.(*slidingWindow)
	}
	w := &slidingWindow{}
	actual, _ := rpmTrackers.LoadOrStore(subject, w)
	return actual.(*slidingWindow)
}

func getTPMTracker(subject string) *tokenWindow {
	if v, ok := tpmTrackers.Load(subject); ok {
		return v.(*tokenWindow)
	}
	w := &tokenWindow{}
	actual, _ := tpmTrackers.LoadOrStore(subject, w)
	return actual.(*tokenWindow)
}

// RecordTokenUsage records token consumption for TPM tracking.
// This should be called by the usage reporter after a request completes.
// subject should be the same key used by QuotaMiddleware (eu:<id> or api key).
func RecordTokenUsage(subject string, totalTokens int64) {
	if subject == "" || totalTokens <= 0 {
		return
	}
	getTPMTracker(subject).add(totalTokens)
}

// RecordTokenUsageForRequest records TPM against the end-user pool when known.
func RecordTokenUsageForRequest(apiKey, endUserID string, totalTokens int64) {
	if totalTokens <= 0 {
		return
	}
	if id := strings.TrimSpace(endUserID); id != "" {
		RecordTokenUsage("eu:"+id, totalTokens)
		return
	}
	RecordTokenUsage(apiKey, totalTokens)
}

// ─── Quota Middleware ───────────────────────────────────────────────────────

// QuotaMiddleware enforces daily-limit, total-quota, RPM (requests per minute),
// TPM (tokens per minute), and spending restrictions for authenticated API keys.
//
// It reads the limits from the accessMetadata set by the auth provider.
// This middleware MUST be placed after AuthMiddleware and before route handlers.
// POST requests are checked; other methods pass unchecked (GET /models etc.
// don't consume quota). The exception is a WebSocket upgrade, whose turns are
// billed like POST requests: see admitWebsocketHandshake. The checks themselves
// live in quota_admission.go and are shared by both transports.
func QuotaMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Enforce on POST requests (actual API calls) and on WebSocket upgrades,
		// whose turns are API calls too. Anything else passes unchecked.
		if c.Request.Method != http.MethodPost {
			if IsWebsocketUpgrade(c.Request) {
				admitWebsocketHandshake(c)
				return
			}
			c.Next()
			return
		}

		apiKey, metadata, ok := quotaCredentials(c)
		if !ok {
			c.Next()
			return
		}
		policy := parseQuotaPolicy(apiKey, metadata)

		// ── Always record this request for system-wide RPM tracking ──
		// This must happen before any metadata checks so ALL authenticated
		// POST requests are counted for the dashboard RPM display.
		getRPMTracker(policy.subject).add()

		if metadata == nil {
			c.Next()
			return
		}

		// Diagnostics, and cached limits for the dashboard snapshot
		policy.record(c)

		// No limits configured — skip all checks
		if !policy.hasLimits() {
			c.Next()
			return
		}

		release, verdict := policy.admit()
		if verdict != nil {
			verdict.abort(c)
			return
		}
		defer release()
		c.Next()
	}
}

// quotaCredentials returns the authenticated API key and the access metadata
// holding its limits.
func quotaCredentials(c *gin.Context) (apiKey string, metadata map[string]string, ok bool) {
	// Get the authenticated API key
	apiKeyVal, exists := c.Get("apiKey")
	if !exists {
		return "", nil, false
	}
	apiKey, ok = apiKeyVal.(string)
	if !ok || apiKey == "" {
		return "", nil, false
	}
	// Get access metadata containing limits (needed for end-user subject).
	if metadataVal, exists := c.Get("accessMetadata"); exists {
		metadata, _ = metadataVal.(map[string]string)
	}
	return apiKey, metadata, true
}

func quotaScope(endUserID string) string {
	if strings.TrimSpace(endUserID) != "" {
		return "account"
	}
	return "key"
}

func stableQuotaSubject(apiKeyID, endUserID string) string {
	if strings.TrimSpace(endUserID) != "" {
		return strings.TrimSpace(endUserID)
	}
	return strings.TrimSpace(apiKeyID)
}

func parsePeriodLimitsMetadata(metadata map[string]string, prefix string) quota.PeriodSpendingLimits {
	return quota.PeriodSpendingLimits{
		FiveHour: parseFloatMetadata(metadata, prefix+"5h"),
		Day:      parseFloatMetadata(metadata, prefix+"day"),
		Week:     parseFloatMetadata(metadata, prefix+"week"),
		Month:    parseFloatMetadata(metadata, prefix+"month"),
	}
}

func hasPeriodLimits(limits quota.PeriodSpendingLimits) bool {
	return limits.FiveHour > 0 || limits.Day > 0 || limits.Week > 0 || limits.Month > 0
}

func formatQuotaNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ─── Usage DB query functions (injected to avoid import cycle) ──────────────

var errQuotaUsageUnavailable = errors.New("quota usage unavailable")

// Key-scoped defaults; InitQuotaUsageFuncs wires real implementations.
// When endUserID is set, InitQuotaUsageFuncs also wires account-scoped aggregators.
var (
	countTodayByKeyFunc     = func(string) (int64, error) { return 0, errQuotaUsageUnavailable }
	countTotalByKeyFunc     = func(string) (int64, error) { return 0, errQuotaUsageUnavailable }
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 0, errQuotaUsageUnavailable }
	queryTodayCostByKeyFunc = func(string) (float64, error) { return 0, errQuotaUsageUnavailable }

	countTodayByEndUserFunc     = func(string) (int64, error) { return 0, errQuotaUsageUnavailable }
	countTotalByEndUserFunc     = func(string) (int64, error) { return 0, errQuotaUsageUnavailable }
	queryTotalCostByEndUserFunc = func(string) (float64, error) { return 0, errQuotaUsageUnavailable }
	queryTodayCostByEndUserFunc = func(string) (float64, error) { return 0, errQuotaUsageUnavailable }
	queryPeriodByKeyFunc        = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{}, errQuotaUsageUnavailable
	}
	queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{}, errQuotaUsageUnavailable
	}
)

// InitQuotaUsageFuncs injects the usage DB query functions into the middleware.
// This avoids a direct import of the usage package which would cause cycles.
func InitQuotaUsageFuncs(
	countToday func(string) (int64, error),
	countTotal func(string) (int64, error),
	totalCost func(string) (float64, error),
	todayCost func(string) (float64, error),
) {
	countTodayByKeyFunc = countToday
	countTotalByKeyFunc = countTotal
	queryTotalCostByKeyFunc = totalCost
	queryTodayCostByKeyFunc = todayCost
}

// InitQuotaEndUserUsageFuncs injects end-user-scoped usage aggregators.
// Owned keys share one account pool for daily/total/spending checks.
func InitQuotaEndUserUsageFuncs(
	countToday func(string) (int64, error),
	countTotal func(string) (int64, error),
	totalCost func(string) (float64, error),
	todayCost func(string) (float64, error),
) {
	countTodayByEndUserFunc = countToday
	countTotalByEndUserFunc = countTotal
	queryTotalCostByEndUserFunc = totalCost
	queryTodayCostByEndUserFunc = todayCost
}

func InitQuotaPeriodUsageFuncs(
	keyUsage func(string, string) (quota.PeriodSpendingUsage, error),
	endUserUsage func(string, string) (quota.PeriodSpendingUsage, error),
) {
	queryPeriodByKeyFunc = keyUsage
	queryPeriodByEndUserFunc = endUserUsage
}

func countTodayUsage(apiKey, endUserID string) (int64, error) {
	if endUserID != "" {
		return countTodayByEndUserFunc(endUserID)
	}
	return countTodayByKeyFunc(apiKey)
}

func countTotalUsage(apiKey, endUserID string) (int64, error) {
	if endUserID != "" {
		return countTotalByEndUserFunc(endUserID)
	}
	return countTotalByKeyFunc(apiKey)
}

func queryTotalCostUsage(apiKey, endUserID string) (float64, error) {
	if endUserID != "" {
		return queryTotalCostByEndUserFunc(endUserID)
	}
	return queryTotalCostByKeyFunc(apiKey)
}

func queryTodayCostUsage(apiKey, endUserID string) (float64, error) {
	if endUserID != "" {
		return queryTodayCostByEndUserFunc(endUserID)
	}
	return queryTodayCostByKeyFunc(apiKey)
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

func acquireKeyConcurrency(apiKey string, limit int) (func(), bool) {
	if apiKey == "" || limit <= 0 {
		return func() {}, true
	}

	inFlightMu.Lock()
	defer inFlightMu.Unlock()

	if inFlightByKey[apiKey] >= limit {
		return nil, false
	}
	inFlightByKey[apiKey]++

	return func() {
		inFlightMu.Lock()
		defer inFlightMu.Unlock()

		current := inFlightByKey[apiKey]
		if current <= 1 {
			delete(inFlightByKey, apiKey)
			return
		}
		inFlightByKey[apiKey] = current - 1
	}, true
}

func keyConcurrencyCount(apiKey string) int {
	if apiKey == "" {
		return 0
	}
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	return inFlightByKey[apiKey]
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
