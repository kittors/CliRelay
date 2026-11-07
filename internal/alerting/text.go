package alerting

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// pick returns the wording for a language. Messages exist in English and
// Simplified Chinese, the two languages the panel's operators write in; the
// structured facts of a message are language-free, so a receiver that wants any
// other wording builds it from those.
func pick(language, en, zhCN string) string {
	if language == LanguageZHCN {
		return zhCN
	}
	return en
}

// usedPercent renders a share that has been used. It rounds down so that a
// window at 99.6% is not announced as 100%: "100% used" says nothing more can
// be spent, which is a different (and louder) statement.
func usedPercent(p float64) string {
	return strconv.Itoa(int(math.Floor(clampFloat(p, 0, 100)+1e-9))) + "%"
}

// leftPercent renders a share that is left, rounding up for the same reason.
func leftPercent(p float64) string {
	return strconv.Itoa(int(math.Ceil(clampFloat(p, 0, 100)-1e-9))) + "%"
}

func clampFloat(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}

// formatDuration writes a span with at most two units: "2d 3h", "5h 30m", "12m".
func formatDuration(d time.Duration, language string) string {
	if d < time.Minute {
		return pick(language, "under a minute", "不到 1 分钟")
	}
	totalMinutes := int(d / time.Minute)
	days, hours, minutes := totalMinutes/(24*60), totalMinutes/60%24, totalMinutes%60
	switch {
	case days > 0:
		return joinUnits(language, days, "d", "天", hours, "h", "小时")
	case hours > 0:
		return joinUnits(language, hours, "h", "小时", minutes, "m", "分钟")
	default:
		return pick(language, fmt.Sprintf("%dm", minutes), fmt.Sprintf("%d 分钟", minutes))
	}
}

func joinUnits(language string, major int, majorEN, majorZH string, minor int, minorEN, minorZH string) string {
	if language == LanguageZHCN {
		if minor == 0 {
			return fmt.Sprintf("%d %s", major, majorZH)
		}
		return fmt.Sprintf("%d %s %d %s", major, majorZH, minor, minorZH)
	}
	if minor == 0 {
		return fmt.Sprintf("%d%s", major, majorEN)
	}
	return fmt.Sprintf("%d%s %d%s", major, majorEN, minor, minorEN)
}

// formatInstant writes a point in time in UTC. The tenant has no time zone of
// its own, and an explicit zone is better than a local time the reader has to
// guess the zone of.
func formatInstant(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// formatAmount writes a usage figure in the unit of its metric.
func formatAmount(metric string, v float64) string {
	switch metric {
	case "cost":
		return fmt.Sprintf("$%.2f", v)
	case "tokens":
		return compactNumber(v)
	default:
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
}

// compactNumber writes 1_234_567 as "1.23M".
func compactNumber(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e9:
		return trimZeros(fmt.Sprintf("%.2f", v/1e9)) + "B"
	case abs >= 1e6:
		return trimZeros(fmt.Sprintf("%.2f", v/1e6)) + "M"
	case abs >= 1e4:
		return trimZeros(fmt.Sprintf("%.1f", v/1e3)) + "K"
	default:
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// spanName names a window by its length, "" when the length is unknown.
func spanName(span time.Duration, language string) string {
	if span <= 0 {
		return ""
	}
	switch {
	case span == 5*time.Hour:
		return pick(language, "5-hour", "5 小时")
	case span == 24*time.Hour:
		return pick(language, "daily", "每日")
	case span == 7*24*time.Hour:
		return pick(language, "weekly", "每周")
	case span >= 28*24*time.Hour && span <= 31*24*time.Hour:
		return pick(language, "monthly", "每月")
	case span%(24*time.Hour) == 0:
		return pick(language, fmt.Sprintf("%d-day", int(span/(24*time.Hour))), fmt.Sprintf("%d 天", int(span/(24*time.Hour))))
	default:
		return pick(language, fmt.Sprintf("%d-hour", int(span.Round(time.Hour)/time.Hour)), fmt.Sprintf("%d 小时", int(span.Round(time.Hour)/time.Hour)))
	}
}

// periodName names a spending window of a key or an end user.
func periodName(window, language string) string {
	switch window {
	case "5h":
		return pick(language, "5-hour", "5 小时")
	case "day":
		return pick(language, "daily", "每日")
	case "week":
		return pick(language, "weekly", "每周")
	case "month":
		return pick(language, "monthly", "每月")
	case "lifetime":
		return pick(language, "lifetime", "累计")
	default:
		return window
	}
}

// metricName names what a spending limit counts.
func metricName(metric, language string) string {
	switch metric {
	case "cost":
		return pick(language, "spending", "花费")
	case "requests":
		return pick(language, "request", "请求")
	case "tokens":
		return pick(language, "token", "Token")
	default:
		return metric
	}
}

// subjectKindName names a kind of subject in running text.
func subjectKindName(kind, language string) string {
	switch kind {
	case SubjectAccount:
		return pick(language, "account", "账号")
	case SubjectKey:
		return pick(language, "API key", "API Key")
	case SubjectEndUser:
		return pick(language, "end user", "终端用户")
	case SubjectChannel:
		return pick(language, "channel", "渠道")
	default:
		return kind
	}
}

// ruleTitle is the headline of a message of rule about count items.
func ruleTitle(rule, language string, count int) string {
	switch rule {
	case RuleQuotaWindow:
		return pick(language, "Account quota is almost used up", "账号额度即将用完")
	case RuleAccountFailure:
		return pick(language, "Upstream account is not working", "上游账号不可用")
	case RuleSpending:
		return pick(language, "Spending limit is almost reached", "花费额度即将用完")
	case RuleUseItOrLoseIt:
		return pick(language, "Quota will reset with most of it unused", "额度即将重置，还剩很多没用")
	case RuleTest:
		return pick(language, "Test message from CliRelay", "CliRelay 测试消息")
	default:
		return rule
	}
}

// groupSubjectName is the subject name of a message about several items.
func groupSubjectName(kind string, count int, language string) string {
	switch kind {
	case SubjectAccount:
		return pick(language, fmt.Sprintf("%d accounts", count), fmt.Sprintf("%d 个账号", count))
	case SubjectKey:
		return pick(language, fmt.Sprintf("%d API keys", count), fmt.Sprintf("%d 个 API Key", count))
	case SubjectEndUser:
		return pick(language, fmt.Sprintf("%d end users", count), fmt.Sprintf("%d 个终端用户", count))
	default:
		return pick(language, fmt.Sprintf("%d items", count), fmt.Sprintf("%d 项", count))
	}
}

// moreLine closes a list that was cut short.
func moreLine(n int, language string) string {
	return pick(language, fmt.Sprintf("… and %d more", n), fmt.Sprintf("……还有 %d 项", n))
}
