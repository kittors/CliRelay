package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

// ─── Admission ──────────────────────────────────────────────────────────────
// A POST and a Responses WebSocket turn both reach the executor and spend
// upstream quota, so both are admitted by the same checks below. Keeping one
// implementation is what stops the two transports from drifting apart.

// quotaPolicy is one key's limits, parsed from the access metadata set by the
// auth provider.
type quotaPolicy struct {
	apiKey    string
	subject   string
	endUserID string
	tenantID  string
	apiKeyID  string

	dailyLimit         int
	totalQuota         int
	concurrencyLimit   int
	rpmLimit           int
	tpmLimit           int
	spendingLimit      float64
	dailySpendingLimit float64
	accountPeriod      quota.PeriodSpendingLimits
	keyPeriod          quota.PeriodSpendingLimits
}

func parseQuotaPolicy(apiKey string, metadata map[string]string) quotaPolicy {
	return quotaPolicy{
		apiKey:             apiKey,
		subject:            quotaSubjectKey(apiKey, metadata),
		endUserID:          strings.TrimSpace(metadata["end-user-id"]),
		tenantID:           strings.TrimSpace(metadata["tenant-id"]),
		apiKeyID:           strings.TrimSpace(metadata["api-key-id"]),
		dailyLimit:         parseIntMetadata(metadata, "daily-limit"),
		totalQuota:         parseIntMetadata(metadata, "total-quota"),
		concurrencyLimit:   parseIntMetadata(metadata, "concurrency-limit"),
		rpmLimit:           parseIntMetadata(metadata, "rpm-limit"),
		tpmLimit:           parseIntMetadata(metadata, "tpm-limit"),
		spendingLimit:      parseFloatMetadata(metadata, "spending-limit"),
		dailySpendingLimit: parseFloatMetadata(metadata, "daily-spending-limit"),
		accountPeriod:      parsePeriodLimitsMetadata(metadata, "account-period-spending-limit-"),
		keyPeriod:          parsePeriodLimitsMetadata(metadata, "key-period-spending-limit-"),
	}
}

func (p *quotaPolicy) hasLimits() bool {
	return p.dailyLimit > 0 || p.totalQuota > 0 || p.concurrencyLimit > 0 || p.rpmLimit > 0 || p.tpmLimit > 0 ||
		p.spendingLimit > 0 || p.dailySpendingLimit > 0 || hasPeriodLimits(p.accountPeriod) || hasPeriodLimits(p.keyPeriod)
}

// record publishes the limits to the request diagnostics and caches the RPM/TPM
// limits for the dashboard snapshot.
func (p *quotaPolicy) record(c *gin.Context) {
	diagnostics.SetQuotaLimits(c, diagnostics.QuotaSnapshot{
		DailyLimit:         p.dailyLimit,
		TotalQuota:         p.totalQuota,
		ConcurrencyLimit:   p.concurrencyLimit,
		RPMLimit:           p.rpmLimit,
		TPMLimit:           p.tpmLimit,
		SpendingLimit:      p.spendingLimit,
		DailySpendingLimit: p.dailySpendingLimit,
	})
	UpdateKeyLimits(p.subject, p.rpmLimit, p.tpmLimit)
}

// admit runs every check for one billable request. When admitted, the caller
// holds the key's concurrency slot (if it has a concurrency limit) until it
// calls release; when refused, no slot is held.
//
// admit does not count the request toward RPM. Callers count it first, because
// the POST path counts every authenticated request for the dashboard, including
// requests of keys that have no limits at all.
func (p *quotaPolicy) admit() (release func(), verdict *quotaVerdict) {
	release = func() {}
	if p.concurrencyLimit > 0 {
		slot, ok := acquireKeyConcurrency(p.subject, p.concurrencyLimit)
		if !ok {
			current := keyConcurrencyCount(p.subject)
			return nil, quotaLimitVerdict("concurrency", float64(p.concurrencyLimit), float64(current), "concurrency_limit_exceeded",
				fmt.Sprintf("Concurrent request limit exceeded: %d in-flight requests (limit %d). Wait for running requests to finish, or raise the concurrency limit in the permission profile.", current, p.concurrencyLimit))
		}
		release = slot
	}
	if verdict = p.checkRates(); verdict == nil {
		verdict = p.checkBudgets()
	}
	if verdict != nil {
		release()
		return nil, verdict
	}
	return release, nil
}

// checkRates runs the per-minute sliding-window checks.
func (p *quotaPolicy) checkRates() *quotaVerdict {
	// --- RPM check (sliding window, in-memory) ---
	if p.rpmLimit > 0 {
		currentRPM := getRPMTracker(p.subject).count()
		if currentRPM > p.rpmLimit {
			return quotaLimitVerdict("rpm", float64(p.rpmLimit), float64(currentRPM), "rpm_limit_exceeded",
				fmt.Sprintf("Requests-per-minute (RPM) limit exceeded: %d/%d requests in the last minute. Slow down, or raise the RPM limit in the permission profile.", currentRPM, p.rpmLimit))
		}
	}

	// --- TPM check (sliding window, in-memory) ---
	if p.tpmLimit > 0 {
		currentTPM := getTPMTracker(p.subject).sum()
		if currentTPM >= int64(p.tpmLimit) {
			return quotaLimitVerdict("tpm", float64(p.tpmLimit), float64(currentTPM), "tpm_limit_exceeded",
				fmt.Sprintf("Tokens-per-minute (TPM) limit exceeded: %d/%d tokens in the last minute. Slow down, or raise the TPM limit in the permission profile.", currentTPM, p.tpmLimit))
		}
	}
	return nil
}

// checkBudgets runs the checks against usage recorded in the usage DB: request
// counts and spending. It changes no counters, so a WebSocket handshake can run
// it without counting as a request. A usage lookup failure refuses (fails closed).
func (p *quotaPolicy) checkBudgets() *quotaVerdict {
	// --- Daily limit check (from usage DB) ---
	if p.dailyLimit > 0 {
		todayCount, err := countTodayUsage(p.apiKey, p.endUserID)
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "day", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if todayCount >= int64(p.dailyLimit) {
			return quotaLimitVerdict("daily", float64(p.dailyLimit), float64(todayCount), "daily_limit_exceeded",
				fmt.Sprintf("Daily request limit exceeded: %d/%d requests used today. Raise the daily request limit in the permission profile, or wait until the next project day.", todayCount, p.dailyLimit))
		}
	}

	// --- Total quota check (from usage DB) ---
	if p.totalQuota > 0 {
		totalCount, err := countTotalUsage(p.apiKey, p.endUserID)
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "lifetime", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if totalCount >= int64(p.totalQuota) {
			return quotaLimitVerdict("total", float64(p.totalQuota), float64(totalCount), "total_quota_exceeded",
				fmt.Sprintf("Total request quota exhausted: %d/%d lifetime requests used. Raise the total request quota in the permission profile to continue.", totalCount, p.totalQuota))
		}
	}

	// --- Spending limit check (from usage DB) ---
	if p.spendingLimit > 0 {
		totalCost, err := queryTotalCostUsage(p.apiKey, p.endUserID)
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "lifetime", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if totalCost >= p.spendingLimit {
			return quotaLimitVerdict("spending", p.spendingLimit, totalCost, "spending_limit_exceeded",
				fmt.Sprintf("Lifetime spending limit exceeded: $%.2f of $%.2f used. Raise the spending limit to continue.", totalCost, p.spendingLimit))
		}
	}

	// --- Daily spending limit check (from usage DB) ---
	if p.dailySpendingLimit > 0 && p.accountPeriod.Day <= 0 && p.keyPeriod.Day <= 0 {
		todayCost, err := queryTodayCostUsage(p.apiKey, p.endUserID)
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "day", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if todayCost >= p.dailySpendingLimit {
			return quotaLimitVerdict("daily_spending", p.dailySpendingLimit, todayCost, "daily_spending_limit_exceeded",
				fmt.Sprintf("Daily spending limit exceeded: $%.2f of $%.2f used today. Raise the daily spending limit in the permission profile, reset today's spending, or wait until the next project day.", todayCost, p.dailySpendingLimit))
		}
	}

	if hasPeriodLimits(p.accountPeriod) {
		used, err := queryPeriodByEndUserFunc(p.tenantID, p.endUserID)
		if err != nil {
			return quotaUsageUnavailableVerdict("account", "period", p.tenantID, p.endUserID, err)
		}
		if verdict := configuredPeriodVerdict("account", p.accountPeriod, used); verdict != nil {
			return verdict
		}
	}
	if hasPeriodLimits(p.keyPeriod) {
		used, err := queryPeriodByKeyFunc(p.tenantID, p.apiKeyID)
		if err != nil {
			return quotaUsageUnavailableVerdict("key", "period", p.tenantID, p.apiKeyID, err)
		}
		if verdict := configuredPeriodVerdict("key", p.keyPeriod, used); verdict != nil {
			return verdict
		}
	}
	return nil
}

func configuredPeriodVerdict(scope string, limits quota.PeriodSpendingLimits, used quota.PeriodSpendingUsage) *quotaVerdict {
	for _, period := range quota.OrderedPeriods {
		limit := limits.Value(period)
		current := used.Value(period)
		if limit <= 0 || current < limit {
			continue
		}
		code := "period_spending_limit_exceeded"
		if period == quota.PeriodDay {
			code = "daily_spending_limit_exceeded"
		}
		scopeLabel := "Key"
		if scope == "account" {
			scopeLabel = "Account"
		}
		message := fmt.Sprintf("%s %s spending limit exceeded: $%.2f of $%.2f used.", scopeLabel, period, current, limit)
		return periodQuotaVerdict(scope, period, limit, current, code, message)
	}
	return nil
}

// ─── Verdicts ───────────────────────────────────────────────────────────────

// quotaVerdict is a refusal from the checks above. The POST middleware writes it
// as the HTTP response; a Responses WebSocket turn sends it as an error event.
// Both carry the same status, JSON error body and X-CliRelay-Quota-* headers.
type quotaVerdict struct {
	status  int
	body    gin.H
	headers []quotaHeader
	// rejection is recorded in the request diagnostics. It is nil when the
	// verdict is not a limit decision (the usage lookup failed).
	rejection *quotaRejectionDiagnostics
}

type quotaHeader struct{ name, value string }

type quotaRejectionDiagnostics struct {
	rejectedBy     string
	limit, current float64
	code, errType  string
	message        string
}

// quotaLimitVerdict is a 429 with a distinct code/message and diagnostic headers.
// Headers help clients that only surface "429 Too Many Requests" after retries.
func quotaLimitVerdict(rejectedBy string, limit, current float64, code, message string) *quotaVerdict {
	const errType = "rate_limit_exceeded"
	return &quotaVerdict{
		status: http.StatusTooManyRequests,
		body: gin.H{
			"error": map[string]interface{}{
				"message": message,
				"type":    errType,
				"code":    code,
			},
		},
		headers: []quotaHeader{
			{"X-CliRelay-Quota-Code", code},
			{"X-CliRelay-Quota-Limit", formatQuotaNumber(limit)},
			{"X-CliRelay-Quota-Current", formatQuotaNumber(current)},
			{"X-CliRelay-Quota-Rejected-By", rejectedBy},
		},
		rejection: &quotaRejectionDiagnostics{rejectedBy: rejectedBy, limit: limit, current: current, code: code, errType: errType, message: message},
	}
}

func periodQuotaVerdict(scope string, period quota.Period, limit, current float64, code, message string) *quotaVerdict {
	const errType = "rate_limit_exceeded"
	return &quotaVerdict{
		status: http.StatusTooManyRequests,
		body: gin.H{"error": gin.H{
			"message": message, "type": errType, "code": code, "scope": scope, "period": period,
		}},
		headers: []quotaHeader{
			{"X-CliRelay-Quota-Code", code},
			{"X-CliRelay-Quota-Rejected-By", "period_spending"},
			{"X-CliRelay-Quota-Scope", scope},
			{"X-CliRelay-Quota-Period", string(period)},
			{"X-CliRelay-Quota-Limit", formatQuotaNumber(limit)},
			{"X-CliRelay-Quota-Current", formatQuotaNumber(current)},
		},
		rejection: &quotaRejectionDiagnostics{rejectedBy: "period_spending", limit: limit, current: current, code: code, errType: errType, message: message},
	}
}

func quotaUsageUnavailableVerdict(scope, period, tenantID, subjectID string, err error) *quotaVerdict {
	log.WithError(err).WithFields(log.Fields{"tenant": tenantID, "scope": scope, "period": period, "subject_id": subjectID}).Error("quota usage query unavailable")
	return &quotaVerdict{
		status: http.StatusServiceUnavailable,
		body: gin.H{"error": gin.H{
			"code": "quota_usage_unavailable", "message": "Quota usage is temporarily unavailable", "scope": scope, "period": period,
		}},
	}
}

// abort writes the verdict as the HTTP response and stops the handler chain.
func (v *quotaVerdict) abort(c *gin.Context) {
	if r := v.rejection; r != nil {
		diagnostics.SetQuotaRejection(c, r.rejectedBy, r.limit, r.current, r.code, r.errType, r.message)
	}
	for _, h := range v.headers {
		c.Header(h.name, h.value)
	}
	c.AbortWithStatusJSON(v.status, v.body)
}

// forTurn renders the verdict for a WebSocket turn, which has no HTTP response
// of its own: the handler sends it to the client as an error event.
func (v *quotaVerdict) forTurn() *handlers.QuotaRejection {
	// The body holds only strings and numbers, so encoding cannot fail.
	body, _ := json.Marshal(v.body)
	headers := make(http.Header, len(v.headers))
	for _, h := range v.headers {
		headers.Set(h.name, h.value)
	}
	return &handlers.QuotaRejection{StatusCode: v.status, Body: body, Headers: headers}
}
