package usage

import (
	"fmt"
	"sort"
	"time"
)

// MonitorLoopGuard counts the requests loop protection ended in the window.
//
// Like the latency percentiles it reads request_logs, because which check ended
// a request is recorded there and not in the rollups, so it only covers the
// retention of those rows; MonitorOverview.Latency.CoverageStart says from when.
// It is always present in a monitor payload of a backend that has loop
// protection, which is how the panel tells one from a backend that has not.
type MonitorLoopGuard struct {
	// ReplyLoops is how many replies were cut, ToolLoops how many requests were
	// answered by the gateway and sent upstream nowhere.
	ReplyLoops int64 `json:"reply_loops"`
	ToolLoops  int64 `json:"tool_loops"`
	// EstimatedSavedCost is the cost, in USD, of the upstream requests that were
	// not made: each answered tool-loop request priced at the average cost of
	// that model's successful requests in the same window and filter. It is an
	// estimate and a modest one: it counts neither the replies that were cut
	// (how long they would have run is unknown) nor the turns the loop would have
	// gone on for. Zero when no model has a priced successful request to average.
	EstimatedSavedCost float64 `json:"estimated_saved_cost"`
}

// monitorLoopGuard reads the counts for [from, to).
func monitorLoopGuard(filter MonitorFilter, from, to time.Time) (MonitorLoopGuard, error) {
	var out MonitorLoopGuard
	db := getReadDB()
	if db == nil {
		return out, nil
	}
	predicate, predicateArgs := filter.detailPredicate()
	window := []any{filter.TenantID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}

	rows, err := db.Query(`SELECT guard_kind, model, COUNT(*)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND guard_kind <> ''`+predicate+`
		GROUP BY guard_kind, model`, append(append([]any{}, window...), predicateArgs...)...)
	if err != nil {
		return out, fmt.Errorf("usage: monitor loop guard counts: %w", err)
	}
	toolByModel := make(map[string]int64)
	for rows.Next() {
		var kind, model string
		var count int64
		if err := rows.Scan(&kind, &model, &count); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("usage: monitor loop guard scan: %w", err)
		}
		switch kind {
		case "reply_loop":
			out.ReplyLoops += count
		case "tool_loop":
			out.ToolLoops += count
			toolByModel[model] += count
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	_ = rows.Close()
	if len(toolByModel) == 0 {
		return out, nil
	}

	models := make([]string, 0, len(toolByModel))
	for model := range toolByModel {
		models = append(models, model)
	}
	sort.Strings(models)
	args := append(append([]any{}, window...), predicateArgs...)
	for _, model := range models {
		args = append(args, model)
	}
	priced, err := db.Query(`SELECT model, COUNT(*), COALESCE(SUM(cost), 0)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND failed = 0 AND cost > 0`+predicate+`
		  AND model IN (`+placeholders(len(models))+`)
		GROUP BY model`, args...)
	if err != nil {
		return out, fmt.Errorf("usage: monitor loop guard average cost: %w", err)
	}
	defer priced.Close()
	for priced.Next() {
		var model string
		var count int64
		var total float64
		if err := priced.Scan(&model, &count, &total); err != nil {
			return out, fmt.Errorf("usage: monitor loop guard average scan: %w", err)
		}
		if count > 0 {
			out.EstimatedSavedCost += float64(toolByModel[model]) * total / float64(count)
		}
	}
	return out, priced.Err()
}
