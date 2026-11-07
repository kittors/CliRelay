package quota

import (
	"errors"
	"math"
	"testing"
)

func TestMatrixValueSetRoundTripsEveryCell(t *testing.T) {
	m := Matrix{}
	n := 1.0
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			m.Set(metric, window, n)
			n++
		}
	}
	n = 1
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if got := m.Value(metric, window); got != n {
				t.Fatalf("%s.%s = %v, want %v", metric, window, got, n)
			}
			n++
		}
	}
	if got := m.Value("bogus", PeriodDay); got != 0 {
		t.Fatalf("unknown metric = %v, want 0", got)
	}
	if got := m.Value(MetricCost, "bogus"); got != 0 {
		t.Fatalf("unknown window = %v, want 0", got)
	}
}

func TestConfiguredCellsKeepsFixedOrderAndSkipsUnlimited(t *testing.T) {
	m := Matrix{}
	m.Set(MetricTokens, PeriodLifetime, 9)
	m.Set(MetricCost, PeriodWeek, 3)
	m.Set(MetricRequests, PeriodFiveHour, 5)
	cells := m.ConfiguredCells()
	want := []Cell{
		{Metric: MetricCost, Window: PeriodWeek, Value: 3},
		{Metric: MetricRequests, Window: PeriodFiveHour, Value: 5},
		{Metric: MetricTokens, Window: PeriodLifetime, Value: 9},
	}
	if len(cells) != len(want) {
		t.Fatalf("cells = %+v, want %+v", cells, want)
	}
	for i := range want {
		if cells[i] != want[i] {
			t.Fatalf("cells[%d] = %+v, want %+v", i, cells[i], want[i])
		}
	}
	if (Matrix{}).IsZero() != true || m.IsZero() {
		t.Fatal("IsZero mismatch")
	}
}

func TestNormalizeQuotaValueRejectsInvalidAndCeilsPositive(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), maxQuotaValue * 2} {
		if _, err := NormalizeQuotaValue(MetricTokens, value); !errors.Is(err, ErrInvalidQuotaValue) {
			t.Fatalf("NormalizeQuotaValue(%v) err = %v, want invalid", value, err)
		}
	}
	if got, err := NormalizeQuotaValue(MetricCost, 10.01); err != nil || got != 11 {
		t.Fatalf("cost 10.01 = %v, %v; want 11", got, err)
	}
	if got, err := NormalizeQuotaValue(MetricRequests, 99.2); err != nil || got != 100 {
		t.Fatalf("requests 99.2 = %v, %v; want 100", got, err)
	}
	if got, err := NormalizeQuotaValue(MetricTokens, 0); err != nil || got != 0 {
		t.Fatalf("zero = %v, %v; want 0", got, err)
	}
}

func TestApplyMatrixPatchOnlyTouchesPresentCellsAndZeroClears(t *testing.T) {
	current := Matrix{}
	current.Set(MetricCost, PeriodDay, 10)
	current.Set(MetricTokens, PeriodMonth, 1000)
	zero, five := 0.0, 5.5
	patched, err := ApplyMatrixPatch(current, &MatrixPatch{
		Cost:     &WindowValuesPatch{Day: &zero},
		Requests: &WindowValuesPatch{FiveHour: &five},
	})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Cost.Day != 0 {
		t.Fatalf("explicit zero must clear, got %v", patched.Cost.Day)
	}
	if patched.Requests.FiveHour != 6 {
		t.Fatalf("requests.5h = %v, want ceil 6", patched.Requests.FiveHour)
	}
	if patched.Tokens.Month != 1000 {
		t.Fatalf("untouched cell changed: %v", patched.Tokens.Month)
	}
	bad := -1.0
	if _, err := ApplyMatrixPatch(current, &MatrixPatch{Tokens: &WindowValuesPatch{Week: &bad}}); !errors.Is(err, ErrInvalidQuotaValue) {
		t.Fatalf("negative patch err = %v", err)
	}
	if !(&MatrixPatch{}).IsEmpty() || (&MatrixPatch{Cost: &WindowValuesPatch{Day: &zero}}).IsEmpty() {
		t.Fatal("IsEmpty mismatch")
	}
}

// 美元四周期仍走旧错误码（前端已有本地化提示），其它格子用新错误。
func TestValidateKeyMatrixWithinAccountErrorKinds(t *testing.T) {
	account, key := Matrix{}, Matrix{}
	account.Set(MetricCost, PeriodWeek, 100)
	key.Set(MetricCost, PeriodWeek, 200)
	var legacy *LimitExceedsAccountError
	if err := ValidateKeyMatrixWithinAccount(key, account); !errors.As(err, &legacy) || legacy.Period != PeriodWeek {
		t.Fatalf("cost week err = %v, want LimitExceedsAccountError", err)
	}

	account, key = Matrix{}, Matrix{}
	account.Set(MetricTokens, PeriodDay, 1000)
	key.Set(MetricTokens, PeriodDay, 2000)
	var cell *QuotaLimitExceedsAccountError
	if err := ValidateKeyMatrixWithinAccount(key, account); !errors.As(err, &cell) || cell.Metric != MetricTokens || cell.Window != PeriodDay {
		t.Fatalf("tokens day err = %v, want QuotaLimitExceedsAccountError", err)
	}

	account, key = Matrix{}, Matrix{}
	account.Set(MetricCost, PeriodLifetime, 50)
	key.Set(MetricCost, PeriodLifetime, 80)
	if err := ValidateKeyMatrixWithinAccount(key, account); !errors.As(err, &cell) || cell.Window != PeriodLifetime {
		t.Fatalf("cost lifetime err = %v, want QuotaLimitExceedsAccountError", err)
	}

	// 任意一侧不限都合法。
	key = Matrix{}
	key.Set(MetricRequests, PeriodMonth, 10)
	if err := ValidateKeyMatrixWithinAccount(key, Matrix{}); err != nil {
		t.Fatalf("unlimited account err = %v", err)
	}
	if err := ValidateKeyMatrixWithinAccount(Matrix{}, account); err != nil {
		t.Fatalf("unlimited key err = %v", err)
	}
}

func TestCapMatrixToAccountLowersOnlyConfiguredOverLimitCells(t *testing.T) {
	account, key := Matrix{}, Matrix{}
	account.Set(MetricCost, PeriodDay, 10)
	account.Set(MetricRequests, PeriodWeek, 100)
	key.Set(MetricCost, PeriodDay, 30)
	key.Set(MetricRequests, PeriodWeek, 50)
	key.Set(MetricTokens, PeriodMonth, 999)
	capped, cells := CapMatrixToAccount("k1", key, account)
	if capped.Cost.Day != 10 || capped.Requests.Week != 50 || capped.Tokens.Month != 999 {
		t.Fatalf("capped = %+v", capped)
	}
	if len(cells) != 1 || cells[0] != (CappedCell{ID: "k1", Metric: MetricCost, Window: PeriodDay, From: 30, To: 10}) {
		t.Fatalf("cells = %+v", cells)
	}
	legacy := LegacyCappedKeys(append(cells, CappedCell{ID: "k1", Metric: MetricTokens, Window: PeriodDay, From: 2, To: 1}))
	if len(legacy) != 1 || legacy[0].Period != PeriodDay {
		t.Fatalf("legacy = %+v", legacy)
	}
}

func TestMatrixPeriodLimitsBridge(t *testing.T) {
	limits := PeriodSpendingLimits{FiveHour: 1, Day: 2, Week: 3, Month: 4}
	if got := PeriodLimitsFromMatrix(MatrixFromPeriodLimits(limits)); got != limits {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestNormalizeQuotaValueBoundsRequestsToIntegerColumn(t *testing.T) {
	if _, err := NormalizeQuotaValue(MetricRequests, float64(math.MaxInt32)+1); !errors.Is(err, ErrInvalidQuotaValue) {
		t.Fatalf("requests over int32 err = %v", err)
	}
	if got, err := NormalizeQuotaValue(MetricTokens, float64(math.MaxInt32)+1); err != nil || got != float64(math.MaxInt32)+1 {
		t.Fatalf("tokens over int32 = %v, %v", got, err)
	}
}

func TestColumnValuesRoundTripThroughLayouts(t *testing.T) {
	cols := ColumnValues{
		Period:       PeriodSpendingLimits{FiveHour: 1, Day: 2, Week: 3, Month: 4},
		LifetimeCost: 50, DailyRequests: 10, TotalRequests: 1000,
	}
	policy := Matrix{}
	policy.Set(MetricTokens, PeriodDay, 777)
	policy.Set(MetricCost, PeriodLifetime, 999) // 账号上该格以列为准

	account := AccountColumnLayout.MatrixFromColumns(cols, policy)
	if account.Cost.Lifetime != 50 || account.Requests.Day != 10 || account.Tokens.Day != 777 || account.Cost.Week != 3 {
		t.Fatalf("account matrix = %+v", account)
	}
	if back := AccountColumnLayout.ColumnsFromMatrix(account); back != cols {
		t.Fatalf("account columns = %+v, want %+v", back, cols)
	}

	profile := ProfileColumnLayout.MatrixFromColumns(cols, policy)
	if profile.Cost.Lifetime != 0 || profile.Requests.Lifetime != 1000 {
		t.Fatalf("profile matrix = %+v", profile)
	}

	// 账号下的 Key 只有美元四周期列：其余格子都来自策略。
	policy.Set(MetricRequests, PeriodDay, 5)
	owned := OwnedKeyColumnLayout.MatrixFromColumns(cols, policy)
	if owned.Requests.Day != 5 || owned.Cost.Lifetime != 999 || owned.Requests.Lifetime != 0 {
		t.Fatalf("owned key matrix = %+v", owned)
	}
	if back := OwnedKeyColumnLayout.ColumnsFromMatrix(owned); back.DailyRequests != 0 || back.LifetimeCost != 0 || back.Period != cols.Period {
		t.Fatalf("owned key columns = %+v", back)
	}
}
