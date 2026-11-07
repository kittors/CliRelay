package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
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

	// access 是这把 Key 的结构化访问策略；没有使用新能力的 Key 为 nil。
	access *accesspolicy.Policy
	// usage 缓存本次准入读到的用量快照，总额度检查与模型策略检查共用。
	usage *policyUsage

	// requestID ties this admission's reservation to the usage records that
	// settle it (see internal/quota/inflight).
	requestID string
	// inflight is this admission's reservation; the budget checks count what
	// the requests admitted before it still hold. Nil outside admit, so a check
	// that admits nothing (a WebSocket handshake) counts no reservations.
	inflight *inflight.Reservation
}

func parseQuotaPolicy(apiKey string, metadata map[string]string) quotaPolicy {
	policy := quotaPolicy{
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
	if access := accesspolicy.Lookup(apiKey); access != nil {
		policy.access = access
		policy.usage = newPolicyUsage(access)
	}
	return policy
}

func (p *quotaPolicy) hasLimits() bool {
	return p.dailyLimit > 0 || p.totalQuota > 0 || p.concurrencyLimit > 0 || p.rpmLimit > 0 || p.tpmLimit > 0 ||
		p.spendingLimit > 0 || p.dailySpendingLimit > 0 || hasPeriodLimits(p.accountPeriod) || hasPeriodLimits(p.keyPeriod) ||
		p.hasPolicyCells()
}

// hasModelRules reports per-model rules, which the model policy middleware
// checks once it knows the model.
func (p *quotaPolicy) hasModelRules() bool {
	return p.access != nil && (len(p.access.KeyRules) > 0 || (p.access.Owned() && len(p.access.AccountRules) > 0))
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
//
// In a cluster the rate and concurrency checks run against counters shared by
// every node (quota_cluster.go). When those are unavailable the checks below
// run on this node's counters with this node's share of each limit; outside a
// cluster the share is the whole limit.
func (p *quotaPolicy) admit() (release func(), verdict *quotaVerdict) {
	// The reservation is taken before the checks read usage, so of two
	// requests admitted at the same moment the later one always sees the
	// earlier one, without holding a lock across the usage reads.
	reservation := p.reserve()
	if release, verdict, ok := p.admitShared(); ok {
		return holdReservation(release, verdict, reservation)
	}
	share := p.nodeShare()
	release, verdict = share.admitLocal()
	return holdReservation(release, verdict, reservation)
}

// holdReservation keeps the reservation of an admitted request until the
// request is released, and drops the reservation of a refused one.
func holdReservation(release func(), verdict *quotaVerdict, reservation *inflight.Reservation) (func(), *quotaVerdict) {
	if verdict != nil {
		reservation.Cancel()
		return nil, verdict
	}
	return func() {
		release()
		reservation.Finish()
	}, nil
}

// quotaLedger holds the reservations of admitted requests; tests replace it.
var quotaLedger = inflight.Default()

// reserve takes this request's reservation on the budgets of its key and of
// the account the key belongs to. A key without a stable id (one defined in
// the config file) is named by its admission subject, as its other counters
// are.
func (p *quotaPolicy) reserve() *inflight.Reservation {
	owner := inflight.Owner{Key: "subject:" + p.subject}
	if p.apiKeyID != "" {
		owner.Key = inflight.KeyName(p.apiKeyID)
	}
	if p.endUserID != "" {
		owner.Account = inflight.AccountName(p.endUserID)
	}
	p.inflight = quotaLedger.Reserve(p.requestID, owner)
	return p.inflight
}

// usageScope is the scope of the budgets that count usage by account when the
// key has one and by key otherwise: the request counts and the lifetime and
// daily spending limits.
func (p *quotaPolicy) usageScope() inflight.Scope {
	if p.endUserID != "" {
		return inflight.ScopeAccount
	}
	return inflight.ScopeKey
}

// admitLocal runs every check against the node-local counters.
func (p *quotaPolicy) admitLocal() (release func(), verdict *quotaVerdict) {
	release = func() {}
	if p.concurrencyLimit > 0 {
		slot, ok := acquireKeyConcurrency(p.subject, p.concurrencyLimit)
		if !ok {
			return nil, concurrencyLimitVerdict(keyConcurrencyCount(p.subject), p.concurrencyLimit)
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
		tracker := getRPMTracker(p.subject)
		currentRPM := tracker.count()
		if currentRPM > p.rpmLimit {
			return rpmLimitVerdict(currentRPM, p.rpmLimit).withRetryAfter(tracker.retryAfter(p.rpmLimit, time.Now()))
		}
	}

	// --- TPM check (sliding window, in-memory) ---
	if p.tpmLimit > 0 {
		tracker := getTPMTracker(p.subject)
		currentTPM := tracker.sum()
		if currentTPM >= int64(p.tpmLimit) {
			return tpmLimitVerdict(currentTPM, p.tpmLimit).withRetryAfter(tracker.retryAfter(int64(p.tpmLimit), time.Now()))
		}
	}
	return nil
}

func concurrencyLimitVerdict(current, limit int) *quotaVerdict {
	return quotaLimitVerdict("concurrency", float64(limit), float64(current), "concurrency_limit_exceeded",
		fmt.Sprintf("Concurrent request limit exceeded: %d in-flight requests (limit %d). Wait for running requests to finish, or raise the concurrency limit in the permission profile.", current, limit))
}

func rpmLimitVerdict(current, limit int) *quotaVerdict {
	return quotaLimitVerdict("rpm", float64(limit), float64(current), "rpm_limit_exceeded",
		fmt.Sprintf("Requests-per-minute (RPM) limit exceeded: %d/%d requests in the last minute. Slow down, or raise the RPM limit in the permission profile.", current, limit))
}

func tpmLimitVerdict(current int64, limit int) *quotaVerdict {
	return quotaLimitVerdict("tpm", float64(limit), float64(current), "tpm_limit_exceeded",
		fmt.Sprintf("Tokens-per-minute (TPM) limit exceeded: %d/%d tokens in the last minute. Slow down, or raise the TPM limit in the permission profile.", current, limit))
}

// checkBudgets runs the checks against usage recorded in the usage DB: request
// counts and spending. It changes no counters, so a WebSocket handshake can run
// it without counting as a request. A usage lookup failure refuses (fails closed).
func (p *quotaPolicy) checkBudgets() *quotaVerdict {
	// Reads that fail fall back to the last successful one for a short while;
	// see quota_outage.go.
	subject, logSubject := p.usageSubjects()
	// Requests admitted before this one and not billed yet count as used: see
	// internal/quota/inflight.
	held := p.inflight.Before(p.usageScope())

	// --- Daily limit check (from usage DB) ---
	if p.dailyLimit > 0 {
		todayCount, err := readQuotaUsage(quotaWindowDayRequests, subject, logSubject, true, func() (int64, error) {
			return countTodayUsage(p.apiKey, p.endUserID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "day", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if current := todayCount + int64(held.Requests); current >= int64(p.dailyLimit) {
			return quotaLimitVerdict("daily", float64(p.dailyLimit), float64(current), "daily_limit_exceeded",
				fmt.Sprintf("Daily request limit exceeded: %d/%d requests used today.%s Raise the daily request limit in the permission profile, or wait until the next project day.", current, p.dailyLimit, inFlightNote(quota.MetricRequests, held))).
				withRetryAfter(windowResetWait(quota.PeriodDay, time.Time{}, time.Now()))
		}
	}

	// --- Total quota check (from usage DB) ---
	if p.totalQuota > 0 {
		totalCount, err := readQuotaUsage(quotaWindowTotalRequests, subject, logSubject, false, func() (int64, error) {
			return countTotalUsage(p.apiKey, p.endUserID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "lifetime", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if current := totalCount + int64(held.Requests); current >= int64(p.totalQuota) {
			return quotaLimitVerdict("total", float64(p.totalQuota), float64(current), "total_quota_exceeded",
				fmt.Sprintf("Total request quota exhausted: %d/%d lifetime requests used.%s Raise the total request quota in the permission profile to continue.", current, p.totalQuota, inFlightNote(quota.MetricRequests, held)))
		}
	}

	// --- Spending limit check (from usage DB) ---
	if p.spendingLimit > 0 {
		totalCost, err := readQuotaUsage(quotaWindowTotalCost, subject, logSubject, false, func() (float64, error) {
			return queryTotalCostUsage(p.apiKey, p.endUserID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "lifetime", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if current := totalCost + held.Cost; current >= p.spendingLimit {
			return quotaLimitVerdict("spending", p.spendingLimit, current, "spending_limit_exceeded",
				fmt.Sprintf("Lifetime spending limit exceeded: $%.2f of $%.2f used.%s Raise the spending limit to continue.", current, p.spendingLimit, inFlightNote(quota.MetricCost, held)))
		}
	}

	// --- Daily spending limit check (from usage DB) ---
	if p.dailySpendingLimit > 0 && p.accountPeriod.Day <= 0 && p.keyPeriod.Day <= 0 {
		todayCost, err := readQuotaUsage(quotaWindowDayCost, subject, logSubject, true, func() (float64, error) {
			return queryTodayCostUsage(p.apiKey, p.endUserID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict(quotaScope(p.endUserID), "day", p.tenantID, stableQuotaSubject(p.apiKeyID, p.endUserID), err)
		} else if current := todayCost + held.Cost; current >= p.dailySpendingLimit {
			return quotaLimitVerdict("daily_spending", p.dailySpendingLimit, current, "daily_spending_limit_exceeded",
				fmt.Sprintf("Daily spending limit exceeded: $%.2f of $%.2f used today.%s Raise the daily spending limit in the permission profile, reset today's spending, or wait until the next project day.", current, p.dailySpendingLimit, inFlightNote(quota.MetricCost, held))).
				withRetryAfter(windowResetWait(quota.PeriodDay, time.Time{}, time.Now()))
		}
	}

	if hasPeriodLimits(p.accountPeriod) {
		// Period windows roll over at local midnight (the 5h window aside), so a
		// reading from another day never stands in.
		used, err := readQuotaUsage(quotaWindowPeriodAccount, "eu:"+p.tenantID+"/"+p.endUserID, "account "+p.endUserID, true, func() (quota.PeriodSpendingUsage, error) {
			return queryPeriodByEndUserFunc(p.tenantID, p.endUserID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict("account", "period", p.tenantID, p.endUserID, err)
		}
		if verdict := configuredPeriodVerdict("account", p.accountPeriod, used, p.inflight.Before(inflight.ScopeAccount)); verdict != nil {
			return verdict
		}
	}
	if hasPeriodLimits(p.keyPeriod) {
		used, err := readQuotaUsage(quotaWindowPeriodKey, "key:"+p.tenantID+"/"+p.apiKeyID, "key "+p.apiKeyID, true, func() (quota.PeriodSpendingUsage, error) {
			return queryPeriodByKeyFunc(p.tenantID, p.apiKeyID)
		})
		if err != nil {
			return quotaUsageUnavailableVerdict("key", "period", p.tenantID, p.apiKeyID, err)
		}
		if verdict := configuredPeriodVerdict("key", p.keyPeriod, used, p.inflight.Before(inflight.ScopeKey)); verdict != nil {
			return verdict
		}
	}
	return p.checkPolicyCells()
}

func configuredPeriodVerdict(scope string, limits quota.PeriodSpendingLimits, used quota.PeriodSpendingUsage, held inflight.Amounts) *quotaVerdict {
	for _, period := range quota.OrderedPeriods {
		limit := limits.Value(period)
		current := used.Value(period) + held.Cost
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
		message := fmt.Sprintf("%s %s spending limit exceeded: $%.2f of $%.2f used.%s", scopeLabel, period, current, limit, inFlightNote(quota.MetricCost, held))
		return periodQuotaVerdict(scope, period, limit, current, code, message).
			withRetryAfter(windowResetWait(period, used.FiveHourWindowStart, time.Now()))
	}
	return nil
}

// inFlightNote says how much of a refused total is held by requests that are
// still running, so a refusal caused by a burst does not read like usage the
// key never had. It is empty when the requests in flight added nothing that
// shows at the precision of the message.
func inFlightNote(metric quota.Metric, held inflight.Amounts) string {
	requests := "requests"
	if held.Requests < 1.5 {
		requests = "request"
	}
	switch {
	case held.Requests < 0.5:
		return ""
	case metric == quota.MetricCost:
		if held.Cost < 0.005 {
			return ""
		}
		return fmt.Sprintf(" This includes about $%.2f reserved by %.0f %s still in flight.", held.Cost, held.Requests, requests)
	case metric == quota.MetricTokens:
		if held.Tokens < 0.5 {
			return ""
		}
		return fmt.Sprintf(" This includes about %.0f tokens reserved by %.0f %s still in flight.", held.Tokens, held.Requests, requests)
	default:
		return fmt.Sprintf(" This includes %.0f %s still in flight.", held.Requests, requests)
	}
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
