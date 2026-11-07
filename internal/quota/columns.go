package quota

import "math"

// ColumnValues 是账号、Key、模板表上既有的固定额度列。它们早于访问策略存在，
// 被重置、门户、CC Switch 用量卡片等大量路径直接读取，所以继续作为对应格子的
// 唯一真源；新格子放在 access_policy 里。各类对象用自己的 ColumnLayout 决定
// 哪些列真正属于它（例如账号下的 Key 只有美元四周期列）。
type ColumnValues struct {
	// Period 是美元四周期：five_hour/daily/weekly/monthly_spending_limit。
	Period PeriodSpendingLimits
	// LifetimeCost 是累计消费限额 spending_limit。
	LifetimeCost float64
	// DailyRequests 是每日请求限额 daily_limit。
	DailyRequests int
	// TotalRequests 是总请求配额 total_quota。
	TotalRequests int
}

// columnsMatrix 把列值投影到矩阵上（不看 layout）。
func columnsMatrix(c ColumnValues) Matrix {
	m := MatrixFromPeriodLimits(c.Period)
	m.Cost.Lifetime = c.LifetimeCost
	m.Requests.Day = float64(c.DailyRequests)
	m.Requests.Lifetime = float64(c.TotalRequests)
	return m
}

// MatrixFromColumns 用 layout 把固定列与策略里的格子合成完整矩阵。
func (l ColumnLayout) MatrixFromColumns(columns ColumnValues, policyLimits Matrix) Matrix {
	return l.OwnedCellsOnly(l.Merge(columnsMatrix(columns), policyLimits))
}

// ColumnsFromMatrix 取出矩阵中由 layout 的固定列承载的部分；不属于 layout 的列为 0。
// 请求次数列是整数，矩阵里的值已经过 NormalizeQuotaValue 向上取整。
func (l ColumnLayout) ColumnsFromMatrix(m Matrix) ColumnValues {
	cols := l.ColumnCellsOnly(m)
	return ColumnValues{
		Period:        PeriodLimitsFromMatrix(cols),
		LifetimeCost:  cols.Cost.Lifetime,
		DailyRequests: clampInt(cols.Requests.Day),
		TotalRequests: clampInt(cols.Requests.Lifetime),
	}
}

func clampInt(value float64) int {
	if value <= 0 || math.IsNaN(value) {
		return 0
	}
	if value >= math.MaxInt32 {
		// daily_limit / total_quota 是 INTEGER 列；超过 int32 的请求次数上限没有实际意义。
		return math.MaxInt32
	}
	return int(math.Ceil(value))
}
