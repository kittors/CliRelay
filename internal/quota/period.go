package quota

import (
	"errors"
	"fmt"
	"math"
	"time"
)

type Period string

const (
	// PeriodFiveHour 是首次消费锚定的固定窗口，不是滚动窗口：窗口在首笔消费时
	// 开启，5 小时后整体失效，之后由下一笔消费重新开窗。与上游 Anthropic 的
	// 5h session window 对齐，也让面板能给出确定的重置时刻。
	PeriodFiveHour Period = "5h"
	PeriodDay      Period = "day"
	PeriodWeek     Period = "week"
	PeriodMonth    Period = "month"
	// PeriodLifetime is the account's cumulative spend. It has no window at all:
	// it only resets when an operator explicitly grants a new allowance, which is
	// why it is deliberately absent from OrderedPeriods (that list drives the four
	// windowed per-period limits) while still being a resettable period.
	PeriodLifetime Period = "lifetime"
)

// FiveHourWindowDuration 是 5h 配额窗口的长度，窗口区间为 [start, start+5h)。
const FiveHourWindowDuration = 5 * time.Hour

var OrderedPeriods = [...]Period{PeriodFiveHour, PeriodDay, PeriodWeek, PeriodMonth}

var (
	ErrInvalidSpendingLimit    = errors.New("invalid spending limit")
	ErrPeriodDayLegacyConflict = errors.New("period day legacy conflict")
	ErrPeriodsRequired         = errors.New("periods must be a non-empty array")
	ErrInvalidPeriod           = errors.New("invalid period")
)

type InvalidPeriodError struct {
	Period Period
}

func (e *InvalidPeriodError) Error() string {
	return fmt.Sprintf("%s: %v", e.Period, ErrInvalidPeriod)
}

func (e *InvalidPeriodError) Unwrap() error {
	return ErrInvalidPeriod
}

// NormalizePeriods validates reset periods and removes duplicates while
// preserving the request order.
func NormalizePeriods(periods []Period) ([]Period, error) {
	if len(periods) == 0 {
		return nil, ErrPeriodsRequired
	}
	out := make([]Period, 0, len(periods))
	seen := make(map[Period]struct{}, len(periods))
	for _, period := range periods {
		switch period {
		case PeriodFiveHour, PeriodDay, PeriodWeek, PeriodMonth, PeriodLifetime:
		default:
			return nil, &InvalidPeriodError{Period: period}
		}
		if _, ok := seen[period]; ok {
			continue
		}
		seen[period] = struct{}{}
		out = append(out, period)
	}
	return out, nil
}

type PeriodSpendingLimits struct {
	FiveHour float64 `json:"5h" yaml:"5h"`
	Day      float64 `json:"day" yaml:"day"`
	Week     float64 `json:"week" yaml:"week"`
	Month    float64 `json:"month" yaml:"month"`
}

type PeriodSpendingLimitsPatch struct {
	FiveHour *float64 `json:"5h"`
	Day      *float64 `json:"day"`
	Week     *float64 `json:"week"`
	Month    *float64 `json:"month"`
}

type CappedKey struct {
	ID     string  `json:"id"`
	Period Period  `json:"period"`
	From   float64 `json:"from"`
	To     float64 `json:"to"`
}

type LimitExceedsAccountError struct {
	Period       Period
	KeyLimit     float64
	AccountLimit float64
}

func (e *LimitExceedsAccountError) Error() string {
	return fmt.Sprintf("Key %s quota $%.0f exceeds account %s quota $%.0f", e.Period, e.KeyLimit, e.Period, e.AccountLimit)
}

func ValidateKeyWithinAccount(keyLimits, accountLimits PeriodSpendingLimits) error {
	for _, period := range OrderedPeriods {
		keyLimit := keyLimits.Value(period)
		accountLimit := accountLimits.Value(period)
		if keyLimit > 0 && accountLimit > 0 && keyLimit > accountLimit {
			return &LimitExceedsAccountError{Period: period, KeyLimit: keyLimit, AccountLimit: accountLimit}
		}
	}
	return nil
}

type PeriodSpending struct {
	Period    Period  `json:"period"`
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	// WindowStart / ResetsAt 仅在锚定窗口（当前只有 5h）且窗口已开启时给出，
	// 供面板显示确定的恢复时刻。日历周期的边界调用方本就能自行推导，不重复下发。
	WindowStart *time.Time `json:"window_start,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

// 已有字段刻意不加 json tag：重置事件表里存有历史快照，改名会让旧记录解析不一致。
type PeriodSpendingUsage struct {
	FiveHour float64
	Day      float64
	Week     float64
	Month    float64
	Lifetime float64
	// FiveHourWindowStart 是当前 5h 窗口的起点（UTC）；零值表示尚未开窗或窗口已过期，
	// 此时 FiveHour 恒为 0。
	FiveHourWindowStart time.Time `json:"five_hour_window_start,omitempty"`
}

func NormalizeWholeUSD(value float64) (float64, error) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: value must be a finite non-negative number", ErrInvalidSpendingLimit)
	}
	if value == 0 {
		return 0, nil
	}
	return math.Ceil(value), nil
}

func NormalizeLimits(limits PeriodSpendingLimits) (PeriodSpendingLimits, error) {
	var err error
	if limits.FiveHour, err = NormalizeWholeUSD(limits.FiveHour); err != nil {
		return PeriodSpendingLimits{}, fmt.Errorf("5h: %w", err)
	}
	if limits.Day, err = NormalizeWholeUSD(limits.Day); err != nil {
		return PeriodSpendingLimits{}, fmt.Errorf("day: %w", err)
	}
	if limits.Week, err = NormalizeWholeUSD(limits.Week); err != nil {
		return PeriodSpendingLimits{}, fmt.Errorf("week: %w", err)
	}
	if limits.Month, err = NormalizeWholeUSD(limits.Month); err != nil {
		return PeriodSpendingLimits{}, fmt.Errorf("month: %w", err)
	}
	return limits, nil
}

func NormalizePatch(patch *PeriodSpendingLimitsPatch) (*PeriodSpendingLimitsPatch, error) {
	if patch == nil {
		return nil, nil
	}
	out := *patch
	for period, ptr := range map[Period]**float64{
		PeriodFiveHour: &out.FiveHour,
		PeriodDay:      &out.Day,
		PeriodWeek:     &out.Week,
		PeriodMonth:    &out.Month,
	} {
		if *ptr == nil {
			continue
		}
		value, err := NormalizeWholeUSD(**ptr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", period, err)
		}
		*ptr = &value
	}
	return &out, nil
}

func ResolveLegacyDay(legacy *float64, patch *PeriodSpendingLimitsPatch) (*PeriodSpendingLimitsPatch, error) {
	normalized, err := NormalizePatch(patch)
	if err != nil {
		return nil, err
	}
	if legacy == nil {
		return normalized, nil
	}
	day, err := NormalizeWholeUSD(*legacy)
	if err != nil {
		return nil, fmt.Errorf("day: %w", err)
	}
	if normalized == nil {
		normalized = &PeriodSpendingLimitsPatch{}
	}
	if normalized.Day != nil && *normalized.Day != day {
		return nil, ErrPeriodDayLegacyConflict
	}
	normalized.Day = &day
	return normalized, nil
}

func ApplyPatch(current PeriodSpendingLimits, patch *PeriodSpendingLimitsPatch) PeriodSpendingLimits {
	if patch == nil {
		return current
	}
	if patch.FiveHour != nil {
		current.FiveHour = *patch.FiveHour
	}
	if patch.Day != nil {
		current.Day = *patch.Day
	}
	if patch.Week != nil {
		current.Week = *patch.Week
	}
	if patch.Month != nil {
		current.Month = *patch.Month
	}
	return current
}

func (l PeriodSpendingLimits) Value(period Period) float64 {
	switch period {
	case PeriodFiveHour:
		return l.FiveHour
	case PeriodDay:
		return l.Day
	case PeriodWeek:
		return l.Week
	case PeriodMonth:
		return l.Month
	default:
		return 0
	}
}

func (u PeriodSpendingUsage) Value(period Period) float64 {
	switch period {
	case PeriodFiveHour:
		return u.FiveHour
	case PeriodDay:
		return u.Day
	case PeriodWeek:
		return u.Week
	case PeriodMonth:
		return u.Month
	case PeriodLifetime:
		return u.Lifetime
	default:
		return 0
	}
}

func BuildPeriodSpending(limits PeriodSpendingLimits, used PeriodSpendingUsage) []PeriodSpending {
	out := make([]PeriodSpending, 0, len(OrderedPeriods))
	for _, period := range OrderedPeriods {
		limit := limits.Value(period)
		if limit <= 0 {
			continue
		}
		current := used.Value(period)
		remaining := limit - current
		if remaining < 0 {
			remaining = 0
		}
		entry := PeriodSpending{Period: period, Limit: limit, Used: current, Remaining: remaining}
		if period == PeriodFiveHour && !used.FiveHourWindowStart.IsZero() {
			windowStart := used.FiveHourWindowStart.UTC()
			resetsAt := windowStart.Add(FiveHourWindowDuration)
			entry.WindowStart = &windowStart
			entry.ResetsAt = &resetsAt
		}
		out = append(out, entry)
	}
	return out
}
