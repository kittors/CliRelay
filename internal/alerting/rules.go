package alerting

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// WindowSample is one quota window of an upstream account as the stored status
// has it.
type WindowSample struct {
	// Key is the upstream's own name for the window (code_week, five_hour, ...).
	Key   string
	Label string
	// Span is the length of the window, 0 when the upstream does not say.
	Span time.Duration
	// UsedPercent is the share already spent, 0 to 100. (The status stores the
	// share remaining; the source converts.)
	UsedPercent float64
	// ResetAt is when the window renews, zero when unknown.
	ResetAt time.Time
	// ObservedAt is when the upstream last confirmed this reading, zero when unknown.
	ObservedAt time.Time
	// AccountWide says the window limits every request of the account, as opposed
	// to one model family or one product of it. Only such windows can be added up
	// into the capacity of a pool of accounts (see BuildPools).
	AccountWide bool
}

// FailureClass says how an account that is not working should be treated.
type FailureClass int

const (
	// FailureNone is a working account.
	FailureNone FailureClass = iota
	// FailureTransient is a timeout or 5xx from the upstream. It usually clears
	// within minutes, so it is only reported once it has lasted.
	FailureTransient
	// FailureNeedsAction is a refusal (401, 402, 403, 404) that waits for a person
	// but that may also be a hiccup, so it too has to last a few minutes before it
	// is reported.
	FailureNeedsAction
	// FailureRevoked is a credential the provider revoked or deactivated. Nothing
	// heals it but signing the account in again, so it is reported at once.
	FailureRevoked
)

// FailureSample describes why an account is not working.
type FailureSample struct {
	Class      FailureClass
	Reason     string
	HTTPStatus int
}

// AccountSample is one upstream account as the rules judge it. Accounts the
// operator switched off are not sampled at all: they are not supposed to work.
type AccountSample struct {
	// ID is the credential's own id.
	ID string
	// Subject identifies the physical account; several credentials may share one,
	// and then share its quota windows.
	Subject  string
	Provider string
	Name     string
	// StaticKey marks a credential that is a plain API key. It has no subscription
	// windows to add up, so the pool view leaves it out.
	StaticKey bool
	Failure   FailureSample
	Windows   []WindowSample
}

// SpendCell is one limit of a key or an end user and how much of it is used.
type SpendCell struct {
	// Metric is "cost", "requests" or "tokens".
	Metric string
	// Window is "5h", "day", "week", "month" or "lifetime".
	Window string
	Used   float64
	Limit  float64
	// ResetAt is when the window renews, zero for a limit that never resets.
	ResetAt time.Time
}

// SpendSample is a key or an end user with the limits it has.
type SpendSample struct {
	// Kind is SubjectKey or SubjectEndUser.
	Kind  string
	ID    string
	Name  string
	Cells []SpendCell
}

const (
	// minQuotaWindow is the shortest window the quota rule reports on. A window of
	// a few hours refills before anyone could act, the scheduler already steers
	// traffic around it, and a busy account would raise the alert every cycle all
	// day. Windows whose length the upstream does not say are kept: they cannot be
	// told from a long one.
	minQuotaWindow = 24 * time.Hour

	// longWindow is what "long" means for the use-it-or-lose-it reminder.
	longWindow = 7 * 24 * time.Hour

	// maxReadingAge is how old a reading may be and still be called "what is left"
	// in a reminder that tells someone to go and spend it. The quota rule needs no
	// such bound: usage only grows within a window, so an old reading can only
	// understate it.
	maxReadingAge = 6 * time.Hour

	epsilon = 1e-9
)

func accountLabel(a AccountSample) string {
	name := cleanText(a.Name)
	if name == "" {
		name = cleanText(a.ID)
	}
	if a.Provider == "" {
		return name
	}
	return cleanText(a.Provider) + " · " + name
}

func formatReset(resetAt, now time.Time, language string) string {
	return fmt.Sprintf(pick(language, "resets in %s (%s)", "将在 %s 后重置（%s）"), formatDuration(resetAt.Sub(now), language), formatInstant(resetAt))
}

func windowDescription(w WindowSample, language string) string {
	if name := spanName(w.Span, language); name != "" {
		return name + pick(language, " window", "窗口")
	}
	if label := cleanText(w.Label); label != "" {
		return label
	}
	return cleanText(w.Key)
}

func windowFacts(a AccountSample, w WindowSample) map[string]any {
	facts := map[string]any{
		"provider":          a.Provider,
		"window_key":        w.Key,
		"window_label":      w.Label,
		"used_percent":      roundTo(w.UsedPercent, 1),
		"remaining_percent": roundTo(100-w.UsedPercent, 1),
	}
	if w.Span > 0 {
		facts["window_seconds"] = int64(w.Span / time.Second)
	}
	if !w.ResetAt.IsZero() {
		facts["reset_at"] = w.ResetAt.UTC().Format(time.RFC3339)
	}
	if a.Subject != "" {
		facts["subject_id"] = a.Subject
	}
	return facts
}

func roundTo(v float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(v*scale) / scale
}

// accountsBySubject keeps one sample per physical account: credentials that
// share an account share its quota windows, and one alert per credential would
// say the same thing twice.
func accountsBySubject(accounts []AccountSample) []AccountSample {
	seen := make(map[string]struct{}, len(accounts))
	out := make([]AccountSample, 0, len(accounts))
	for _, account := range accounts {
		id := account.Subject
		if id == "" {
			id = account.ID
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, account)
	}
	return out
}

func accountSubject(a AccountSample) Subject {
	id := a.Subject
	if id == "" {
		id = a.ID
	}
	return Subject{Kind: SubjectAccount, ID: id, Name: cleanText(a.Name)}
}

// quotaCandidates finds the accounts whose quota windows have used the
// threshold share.
//
// An alert is about one window of one account. Its key holds no reset time:
// upstreams that report "resets in N seconds" give a time that drifts by a second
// or two from probe to probe, and a key that moved with it would raise the same
// alert on every probe. The engine instead keeps the alert quiet until the
// window's reset (plus a grace for that drift). A window that has renewed since
// it was read says nothing about the new cycle and is skipped.
func quotaCandidates(rule QuotaWindowRule, accounts []AccountSample, now time.Time) []Candidate {
	var out []Candidate
	for _, account := range accountsBySubject(accounts) {
		for _, w := range account.Windows {
			if w.Span > 0 && w.Span < minQuotaWindow {
				continue
			}
			if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
				continue
			}
			if w.UsedPercent+epsilon < float64(rule.ThresholdPercent) {
				continue
			}
			severity := SeverityWarning
			if w.UsedPercent+epsilon >= 100 {
				severity = SeverityCritical
			}
			out = append(out, Candidate{
				Rule:     RuleQuotaWindow,
				Severity: severity,
				Key:      strings.Join([]string{RuleQuotaWindow, accountSubject(account).ID, w.Key}, "|"),
				Subject:  accountSubject(account),
				Facts:    withThreshold(windowFacts(account, w), rule.ThresholdPercent),
				Until:    w.ResetAt,
				Describe: func(language string) string {
					text := fmt.Sprintf(pick(language, "%s: %s is %s used (%s left)", "%s：%s已用 %s（剩余 %s）"),
						accountLabel(account), windowDescription(w, language), usedPercent(w.UsedPercent), leftPercent(100-w.UsedPercent))
					if !w.ResetAt.IsZero() {
						text += pick(language, ", ", "，") + formatReset(w.ResetAt, now, language)
					}
					return text
				},
			})
		}
	}
	return out
}

func withThreshold(facts map[string]any, threshold int) map[string]any {
	facts["threshold_percent"] = threshold
	return facts
}

// useItOrLoseItCandidates finds the long windows that are about to renew with a
// lot of them unspent. The reminder is a courtesy: it is only sent for a reading
// recent enough to trust, and never for an account that is not working (it could
// not spend the allowance anyway; the failure alert covers it).
func useItOrLoseItCandidates(rule UseItOrLoseItRule, accounts []AccountSample, now time.Time) []Candidate {
	var out []Candidate
	for _, account := range accountsBySubject(accounts) {
		if account.Failure.Class != FailureNone {
			continue
		}
		for _, w := range account.Windows {
			if w.Span < longWindow || w.ResetAt.IsZero() || !w.ResetAt.After(now) {
				continue
			}
			if w.ObservedAt.IsZero() || now.Sub(w.ObservedAt) > maxReadingAge {
				continue
			}
			remaining := 100 - w.UsedPercent
			untilReset := w.ResetAt.Sub(now)
			if remaining+epsilon < float64(rule.MinRemainingPercent) || untilReset > time.Duration(rule.HoursBeforeReset)*time.Hour {
				continue
			}
			facts := windowFacts(account, w)
			facts["min_remaining_percent"] = rule.MinRemainingPercent
			facts["hours_before_reset"] = rule.HoursBeforeReset
			out = append(out, Candidate{
				Rule:     RuleUseItOrLoseIt,
				Severity: SeverityInfo,
				Key:      strings.Join([]string{RuleUseItOrLoseIt, accountSubject(account).ID, w.Key}, "|"),
				Subject:  accountSubject(account),
				Facts:    facts,
				Until:    w.ResetAt,
				Describe: func(language string) string {
					return fmt.Sprintf(pick(language,
						"%s: the %s still has %s left and %s. Use it before it is lost",
						"%s：%s还剩 %s，且%s。不用就浪费了"),
						accountLabel(account), windowDescription(w, language), leftPercent(remaining), formatReset(w.ResetAt, now, language))
				},
			})
		}
	}
	return out
}

// failureKeyPrefix is the dedupe key prefix of failure alerts, for clearing the
// records of accounts that recovered (see SentState.forgetFailures).
const failureKeyPrefix = RuleAccountFailure + "|"

// failureCandidates finds the accounts that have not been working for long
// enough to report. since is the engine's memory of when each account was first
// seen failing; it is updated here: an account that works again is forgotten, so
// its next failure starts a new clock.
//
// The clock is the engine's rather than the credential's own timestamps because
// those move with every failed request, and so say when it last failed, not when
// it started. The cost is that a node that has just become the leader starts
// every clock again, which delays an alert, never invents one.
func failureCandidates(rule AccountFailureRule, accounts []AccountSample, since map[string]time.Time, now time.Time) (candidates []Candidate, failing map[string]struct{}) {
	failing = make(map[string]struct{})
	failingNow := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if account.Failure.Class == FailureNone {
			continue
		}
		failingNow[account.ID] = struct{}{}
		failing[failureKeyPrefix+account.ID] = struct{}{}
		started, known := since[account.ID]
		if !known {
			started = now
			since[account.ID] = now
		}
		lasted := now.Sub(started)
		if account.Failure.Class != FailureRevoked && lasted < time.Duration(rule.ForMinutes)*time.Minute {
			continue
		}
		severity := SeverityWarning
		if account.Failure.Class == FailureRevoked {
			severity = SeverityCritical
		}
		facts := map[string]any{
			"provider":        account.Provider,
			"reason":          account.Failure.Reason,
			"failing_since":   started.UTC().Format(time.RFC3339),
			"failing_minutes": int(lasted / time.Minute),
			"credential_id":   account.ID,
		}
		if account.Failure.HTTPStatus > 0 {
			facts["http_status"] = account.Failure.HTTPStatus
		}
		if account.Subject != "" {
			facts["subject_id"] = account.Subject
		}
		candidates = append(candidates, Candidate{
			Rule:     RuleAccountFailure,
			Severity: severity,
			Key:      failureKeyPrefix + account.ID,
			Subject:  Subject{Kind: SubjectAccount, ID: account.ID, Name: cleanText(account.Name)},
			Facts:    facts,
			Describe: func(language string) string {
				text := fmt.Sprintf(pick(language, "%s: %s", "%s：%s"), accountLabel(account), failureText(account.Failure, language))
				if lasted >= time.Minute {
					text += pick(language, ", failing for ", "，已持续 ") + formatDuration(lasted, language)
				}
				return text
			},
		})
	}
	for id := range since {
		if _, still := failingNow[id]; !still {
			delete(since, id)
		}
	}
	return candidates, failing
}

// failureText explains a failure and, where there is one, what to do about it.
func failureText(f FailureSample, language string) string {
	switch f.Reason {
	case "reauth_required":
		return pick(language, "the credential was revoked or deactivated; sign the account in again", "凭据已被撤销或停用，需要重新登录该账号")
	case "unauthorized":
		return pick(language, "the upstream rejects the credential (HTTP 401)", "上游拒绝了该凭据（HTTP 401）")
	case "payment_required":
		return pick(language, "the upstream refuses it for billing or permission reasons (HTTP 402/403)", "上游因计费或权限拒绝了请求（HTTP 402/403）")
	case "not_found":
		return pick(language, "the upstream answers 404", "上游返回 404")
	case "upstream_error":
		return pick(language, "the upstream keeps failing (timeouts or 5xx)", "上游持续出错（超时或 5xx）")
	default:
		return pick(language, "the account is not usable", "该账号当前不可用")
	}
}

// spendingCandidates finds the keys and end users that have used the threshold
// share of one of their limits. Limits of every window count, the lifetime one
// included: it never resets, so it is reported again once per cooldown for as
// long as it stays that close, which is the point of a limit that only a person
// can lift.
func spendingCandidates(rule SpendingRule, spenders []SpendSample, now time.Time) []Candidate {
	var out []Candidate
	for _, spender := range spenders {
		for _, cell := range spender.Cells {
			if cell.Limit <= 0 {
				continue
			}
			share := cell.Used / cell.Limit * 100
			if share+epsilon < float64(rule.ThresholdPercent) {
				continue
			}
			severity := SeverityWarning
			if share+epsilon >= 100 {
				severity = SeverityCritical
			}
			facts := map[string]any{
				"metric":            cell.Metric,
				"window":            cell.Window,
				"used":              roundTo(cell.Used, 4),
				"limit":             cell.Limit,
				"used_percent":      roundTo(share, 1),
				"threshold_percent": rule.ThresholdPercent,
			}
			if !cell.ResetAt.IsZero() {
				facts["reset_at"] = cell.ResetAt.UTC().Format(time.RFC3339)
			}
			out = append(out, Candidate{
				Rule:     RuleSpending,
				Severity: severity,
				Key:      strings.Join([]string{RuleSpending, spender.Kind, spender.ID, cell.Metric, cell.Window, strconv.FormatFloat(cell.Limit, 'f', -1, 64)}, "|"),
				Subject:  Subject{Kind: spender.Kind, ID: spender.ID, Name: cleanText(spender.Name)},
				Facts:    facts,
				Until:    cell.ResetAt,
				Describe: func(language string) string {
					kind, name := subjectKindName(spender.Kind, language), cleanText(spender.Name)
					period, metric := periodName(cell.Window, language), metricName(cell.Metric, language)
					used, limit := formatAmount(cell.Metric, cell.Used), formatAmount(cell.Metric, cell.Limit)
					var text string
					if language == LanguageZHCN {
						text = fmt.Sprintf("%s「%s」：%s%s限额已用 %s（%s / %s）", kind, name, period, metric, usedPercent(share), used, limit)
					} else {
						text = fmt.Sprintf("%s \"%s\": %s of the %s %s limit is used (%s of %s)", kind, name, usedPercent(share), period, metric, used, limit)
					}
					if !cell.ResetAt.IsZero() {
						text += pick(language, "; ", "；") + formatReset(cell.ResetAt, now, language)
					} else {
						text += pick(language, "; this limit does not reset", "；该限额不会自动重置")
					}
					return text
				},
			})
		}
	}
	return out
}
