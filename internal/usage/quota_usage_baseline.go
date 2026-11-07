package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	log "github.com/sirupsen/logrus"
)

// 重置一个周期，就是把该窗口里「到此刻为止」的用量记成基线，之后的有效用量 =
// 原始用量 − 基线。美元总额的基线早已存在 period_spending_resets.cost_baseline；
// 本文件补上其余维度：总请求数、总 Token，以及按模型的三项用量，存放在同一行的
// usage_baseline 列（JSON）。这样一次重置同时让总额度和按模型规则在该窗口内清零，
// 管理员不会遇到「重置后总额度恢复了、某个模型规则依然满额」的情况。

// windowUsageBaseline 是一次重置记下的某个窗口的非美元总额用量。
type windowUsageBaseline struct {
	// Window 是写入时的窗口标识。基线行的 window_key 列可能被不认识本字段的旧版本
	// 节点（滚动升级期间）单独改写；只有两者一致时这份基线才可信。
	Window   string                   `json:"window"`
	Requests float64                  `json:"requests,omitempty"`
	Tokens   float64                  `json:"tokens,omitempty"`
	Models   map[string]modelBaseline `json:"models,omitempty"`
}

type modelBaseline struct {
	Cost     float64 `json:"cost,omitempty"`
	Requests float64 `json:"requests,omitempty"`
	Tokens   float64 `json:"tokens,omitempty"`
}

func ensurePeriodSpendingResetUsageBaselineColumn(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec("ALTER TABLE period_spending_resets ADD COLUMN usage_baseline TEXT NOT NULL DEFAULT ''"); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Warnf("usage: migrate period_spending_resets.usage_baseline: %v", err)
		}
	}
}

// usageBaselineFromSnapshot 取出快照在 window 上需要记作基线的部分。
func usageBaselineFromSnapshot(snapshot QuotaUsageSnapshot, window quota.Period, windowKey string) windowUsageBaseline {
	baseline := windowUsageBaseline{
		Window:   windowKey,
		Requests: snapshot.Total.Value(quota.MetricRequests, window),
		Tokens:   snapshot.Total.Value(quota.MetricTokens, window),
	}
	for model, used := range snapshot.Models {
		item := modelBaseline{
			Cost:     used.Value(quota.MetricCost, window),
			Requests: used.Value(quota.MetricRequests, window),
			Tokens:   used.Value(quota.MetricTokens, window),
		}
		if item == (modelBaseline{}) {
			continue
		}
		if baseline.Models == nil {
			baseline.Models = make(map[string]modelBaseline)
		}
		baseline.Models[model] = item
	}
	return baseline
}

func encodeUsageBaseline(baseline windowUsageBaseline) string {
	data, err := json.Marshal(baseline)
	if err != nil {
		return ""
	}
	return string(data)
}

// applyTo 从快照的 window 列上减去基线。美元总额不在这里处理（见 cost_baseline）。
func (b windowUsageBaseline) applyTo(snapshot *QuotaUsageSnapshot, window quota.Period) {
	snapshot.Total.Set(quota.MetricRequests, window, nonNegative(snapshot.Total.Value(quota.MetricRequests, window)-b.Requests))
	snapshot.Total.Set(quota.MetricTokens, window, nonNegative(snapshot.Total.Value(quota.MetricTokens, window)-b.Tokens))
	for model, base := range b.Models {
		used, ok := snapshot.Models[model]
		if !ok {
			continue
		}
		used.Set(quota.MetricCost, window, nonNegative(used.Value(quota.MetricCost, window)-base.Cost))
		used.Set(quota.MetricRequests, window, nonNegative(used.Value(quota.MetricRequests, window)-base.Requests))
		used.Set(quota.MetricTokens, window, nonNegative(used.Value(quota.MetricTokens, window)-base.Tokens))
		snapshot.Models[model] = used
	}
}

// listPeriodUsageBaselines 读取仍处于当前窗口的非美元基线。
func listPeriodUsageBaselines(tenantID string, subject periodSubject, ids []string, windows subjectPeriodWindows) (map[string]map[quota.Period]windowUsageBaseline, error) {
	out := make(map[string]map[quota.Period]windowUsageBaseline)
	if len(ids) == 0 {
		return out, nil
	}
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	subjectType, err := periodResetSubjectType(subject)
	if err != nil {
		return nil, err
	}
	query := `SELECT subject_id, period, window_key, usage_baseline FROM period_spending_resets
		WHERE tenant_id = ? AND subject_type = ? AND usage_baseline <> '' AND subject_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+2)
	args = append(args, normalizeTenantID(tenantID), subjectType)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, rawPeriod, windowKey, raw string
		if err := rows.Scan(&id, &rawPeriod, &windowKey, &raw); err != nil {
			return nil, err
		}
		period := quota.Period(rawPeriod)
		expected := windows.windowKey(id, period)
		if expected == "" || strings.TrimSpace(windowKey) != expected {
			continue
		}
		var baseline windowUsageBaseline
		if err := json.Unmarshal([]byte(raw), &baseline); err != nil {
			// 损坏的基线只会让该窗口的非美元用量少减一些（偏保守，不会多放行），
			// 记一条日志即可，不能因此拒绝整个准入。
			log.WithError(err).Warnf("usage: ignore unreadable usage baseline for %s %s %s", subjectType, id, period)
			continue
		}
		if baseline.Window != expected {
			continue
		}
		if out[id] == nil {
			out[id] = make(map[quota.Period]windowUsageBaseline)
		}
		out[id][period] = baseline
	}
	return out, rows.Err()
}

// writeUsageBaselinesTx 在重置事务中为每个被重置的周期写入非美元基线。行由同一
// 事务里先执行的 cost_baseline UPSERT 保证存在。
func writeUsageBaselinesTx(tx *sql.Tx, tenantID, subjectType, subjectID string, snapshot QuotaUsageSnapshot, windowKeys map[quota.Period]string) error {
	for period, windowKey := range windowKeys {
		encoded := encodeUsageBaseline(usageBaselineFromSnapshot(snapshot, period, windowKey))
		if _, err := tx.Exec(`UPDATE period_spending_resets SET usage_baseline = ?
			WHERE tenant_id = ? AND subject_type = ? AND subject_id = ? AND period = ?`,
			encoded, tenantID, subjectType, subjectID, string(period)); err != nil {
			return fmt.Errorf("usage: write %s usage baseline: %w", period, err)
		}
	}
	return nil
}
