package quota

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestResolveLegacyDayCompatibilityAndConflict(t *testing.T) {
	legacy := 10.1
	day := 11.0
	patch, err := ResolveLegacyDay(&legacy, &PeriodSpendingLimitsPatch{Day: &day})
	if err != nil || patch == nil || patch.Day == nil || *patch.Day != 11 {
		t.Fatalf("same normalized day = %#v, err=%v", patch, err)
	}
	conflict := 12.0
	if _, err := ResolveLegacyDay(&legacy, &PeriodSpendingLimitsPatch{Day: &conflict}); !errors.Is(err, ErrPeriodDayLegacyConflict) {
		t.Fatalf("conflict err = %v, want ErrPeriodDayLegacyConflict", err)
	}
}

func TestNormalizeWholeUSDRejectsInvalidAndCeilsPositive(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := NormalizeWholeUSD(value); !errors.Is(err, ErrInvalidSpendingLimit) {
			t.Fatalf("NormalizeWholeUSD(%v) err=%v, want invalid", value, err)
		}
	}
	if got, err := NormalizeWholeUSD(10.01); err != nil || got != 11 {
		t.Fatalf("NormalizeWholeUSD = %v, %v; want 11,nil", got, err)
	}
}

// 固定窗口下 5h 必须给出窗口起止时刻，面板据此显示恢复倒计时；
// 日历周期不下发，避免调用方误以为它们也由服务端锚定。
func TestBuildPeriodSpendingExposesFiveHourWindowOnly(t *testing.T) {
	windowStart := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	limits := PeriodSpendingLimits{FiveHour: 50, Day: 100}
	items := BuildPeriodSpending(limits, PeriodSpendingUsage{
		FiveHour: 12, Day: 30, FiveHourWindowStart: windowStart,
	})
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	five, day := items[0], items[1]
	if five.Period != PeriodFiveHour || day.Period != PeriodDay {
		t.Fatalf("unexpected order: %+v", items)
	}
	if five.WindowStart == nil || !five.WindowStart.Equal(windowStart) {
		t.Fatalf("5h window start = %v, want %v", five.WindowStart, windowStart)
	}
	if five.ResetsAt == nil || !five.ResetsAt.Equal(windowStart.Add(FiveHourWindowDuration)) {
		t.Fatalf("5h resets at = %v, want %v", five.ResetsAt, windowStart.Add(FiveHourWindowDuration))
	}
	if day.WindowStart != nil || day.ResetsAt != nil {
		t.Fatalf("calendar period must not carry anchored window: %+v", day)
	}
}

// 没有活跃窗口时不能给出重置时刻，否则前端会显示一个并不存在的倒计时。
func TestBuildPeriodSpendingOmitsWindowWhenNotOpen(t *testing.T) {
	items := BuildPeriodSpending(PeriodSpendingLimits{FiveHour: 50}, PeriodSpendingUsage{})
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].WindowStart != nil || items[0].ResetsAt != nil {
		t.Fatalf("window must be absent without an anchor: %+v", items[0])
	}
}
