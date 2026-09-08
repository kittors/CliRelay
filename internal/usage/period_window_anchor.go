package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// 5 小时配额窗口是「首次消费锚定的固定窗口」，不是滚动窗口：
// 窗口区间为 [window_start, window_start+5h)，窗口内用量累计，窗口过期后
// 由下一笔实际消费开启新窗口（间隔期不计时）。这与上游 Anthropic 的
// 5h session window 语义一致，也让面板能给出确定的重置时刻；滚动窗口
// 做不到这一点，用户被限流后无法知道何时恢复。
//
// day/week/month 仍然是日历对齐的固定窗口，锚点机制只服务 5h。
// 与 usage_rollup_buckets 的 quota_minute_utc 桶键同格式，锚点可直接参与
// bucket_start 的字符串比较，无需转换。固定宽度保证字典序等于时间序。
const quotaMinuteKeyLayout = "2006-01-02T15:04"

const periodWindowAnchorsTableSQL = `
CREATE TABLE IF NOT EXISTS period_window_anchors (
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  subject_type  TEXT NOT NULL,
  subject_id    TEXT NOT NULL,
  period        TEXT NOT NULL,
  window_start  TEXT NOT NULL,
  updated_at    TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, subject_type, subject_id, period)
);
`

func ensurePeriodWindowAnchors(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(periodWindowAnchorsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure period_window_anchors: %w", err)
	}
	return nil
}

// quotaMinuteKey 把时刻截断到 UTC 分钟，得到与消费桶对齐的窗口键。
func quotaMinuteKey(at time.Time) string {
	return at.UTC().Truncate(time.Minute).Format(quotaMinuteKeyLayout)
}

// fiveHourExpiryThreshold 返回「锚点早于等于该键即视为窗口已过期」的边界。
// 窗口 [anchor, anchor+5h) 过期等价于 now >= anchor+5h，即 anchor <= now-5h。
func fiveHourExpiryThreshold(now time.Time) string {
	return quotaMinuteKey(now.Add(-quota.FiveHourWindowDuration))
}

// touchFiveHourWindowAnchorTx 在首次消费时开窗，窗口未过期则保持原锚点不动。
// 条件 UPDATE 让并发落账天然幂等：同一窗口内的后续消费不会推移窗口起点。
func touchFiveHourWindowAnchorTx(tx *sql.Tx, tenantID, subjectType, subjectID string, at time.Time) error {
	subjectID = strings.TrimSpace(subjectID)
	if tx == nil || subjectID == "" {
		return nil
	}
	windowStart := quotaMinuteKey(at)
	_, err := tx.Exec(`
		INSERT INTO period_window_anchors
		 (tenant_id, subject_type, subject_id, period, window_start, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, subject_type, subject_id, period) DO UPDATE SET
		  window_start = excluded.window_start,
		  updated_at = excluded.updated_at
		 WHERE period_window_anchors.window_start <= ?
	`, normalizeTenantID(tenantID), subjectType, subjectID, string(quota.PeriodFiveHour),
		windowStart, at.UTC().Format(time.RFC3339Nano), fiveHourExpiryThreshold(at))
	if err != nil {
		return fmt.Errorf("usage: upsert 5h window anchor: %w", err)
	}
	return nil
}

// listActiveFiveHourAnchors 返回仍在有效期内的窗口起点；已过期或从未开窗的
// subject 不会出现在结果里，调用方据此把 5h 用量视为 0。
func listActiveFiveHourAnchors(tenantID string, subject periodSubject, ids []string, now time.Time) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// 与用量查询用同一个分片大小，避免 IN 列表撑爆驱动的参数上限。
	if len(ids) > subjectChunkSize {
		for start := 0; start < len(ids); start += subjectChunkSize {
			end := start + subjectChunkSize
			if end > len(ids) {
				end = len(ids)
			}
			part, err := listActiveFiveHourAnchors(tenantID, subject, ids[start:end], now)
			if err != nil {
				return nil, err
			}
			for id, anchor := range part {
				out[id] = anchor
			}
		}
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
	query := `SELECT subject_id, window_start FROM period_window_anchors
		WHERE tenant_id = ? AND subject_type = ? AND period = ? AND window_start > ?
		  AND subject_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+4)
	args = append(args, normalizeTenantID(tenantID), subjectType, string(quota.PeriodFiveHour), fiveHourExpiryThreshold(now))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: query 5h anchors: %v", ErrQuotaUsageUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, windowStart string
		if err := rows.Scan(&id, &windowStart); err != nil {
			return nil, fmt.Errorf("%w: scan 5h anchor: %v", ErrQuotaUsageUnavailable, err)
		}
		out[id] = windowStart
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows 5h anchors: %v", ErrQuotaUsageUnavailable, err)
	}
	return out, nil
}

// parseQuotaMinuteKey 把窗口键还原成时刻，用于对外暴露窗口起止时间。
func parseQuotaMinuteKey(key string) (time.Time, bool) {
	parsed, err := time.ParseInLocation(quotaMinuteKeyLayout, strings.TrimSpace(key), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
