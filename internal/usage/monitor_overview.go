package usage

import (
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// QueryMonitorOverview assembles the monitor center payload for rangeKey.
//
// Totals and the series are the core of the page: if they fail, the request
// fails. Every other section is secondary and degrades to an empty value with
// a warning, so one slow or broken query (say request_logs during cleanup)
// does not blank the whole page. The shape is always complete — empty lists,
// never null — so the panel needs no null checks.
func QueryMonitorOverview(filter MonitorFilter, rangeKey string, now time.Time) (MonitorOverview, error) {
	filter = filter.normalized()
	key, ok := NormalizeMonitorRange(rangeKey)
	if !ok {
		key = MonitorDefaultRange
	}
	w := resolveMonitorWindow(key, now, getUsageLocation())
	out := emptyMonitorOverview(w)
	if getReadDB() == nil {
		return out, nil
	}

	aggs, current, err := monitorSeriesAggs(w, filter)
	if err != nil {
		return out, err
	}
	previous, err := monitorPeriodTotal(w, filter, w.prevStart, w.prevEnd)
	if err != nil {
		return out, err
	}
	out.Series = monitorSeriesPoints(w, aggs)
	out.Summary = MonitorSummary{Current: current.totals(), Previous: previous.totals()}

	names := newMonitorConsumerNames(filter.TenantID)
	empty := emptyMonitorOverview(w)
	models := monitorSection("models", nil, func() ([]*monitorFolded, error) {
		return monitorStageOne(w, filter, monitorModelDimension)
	})
	channels := monitorSection("channels", nil, func() ([]*monitorFolded, error) {
		return monitorStageOne(w, filter, monitorChannelDimension)
	})
	consumers := monitorSection("consumers", nil, func() ([]*monitorFolded, error) {
		return monitorStageOne(w, filter, monitorConsumerDimension)
	})
	out.Models = monitorBreakdownRows(w, filter, monitorModelDimension, models)
	out.Channels = monitorBreakdownRows(w, filter, monitorChannelDimension, channels)
	for i := range out.Channels.Rows {
		row := &out.Channels.Rows[i]
		row.Provider, row.AuthType = InferChannelDisplayMeta(row.Label, "", "", "")
	}
	out.Consumers = monitorBreakdownRows(w, filter, monitorConsumerDimension, consumers)
	for i := range out.Consumers.Rows {
		row := &out.Consumers.Rows[i]
		name := names.resolve(row.Key)
		row.Label, row.Kind, row.KeyHint = name.label, name.kind, name.hint
	}

	// Filter options ignore the active filters, so the unfiltered stage-one
	// aggregates are reused when nothing is selected.
	if filter.active() {
		all := filter.unfiltered()
		models = monitorSection("model options", nil, func() ([]*monitorFolded, error) {
			return monitorStageOne(w, all, monitorModelDimension)
		})
		channels = monitorSection("channel options", nil, func() ([]*monitorFolded, error) {
			return monitorStageOne(w, all, monitorChannelDimension)
		})
		consumers = monitorSection("consumer options", nil, func() ([]*monitorFolded, error) {
			return monitorStageOne(w, all, monitorConsumerDimension)
		})
	}
	out.Filters = MonitorFilterOptions{
		Models: monitorFilterOptionsFrom(models, func(item *monitorFolded) MonitorFilterOption {
			return MonitorFilterOption{Value: item.key, Label: item.label, Requests: item.agg.Requests}
		}),
		Channels: monitorFilterOptionsFrom(channels, func(item *monitorFolded) MonitorFilterOption {
			provider, _ := InferChannelDisplayMeta(item.label, "", "", "")
			return MonitorFilterOption{
				Value: item.key, Label: item.label, Requests: item.agg.Requests,
				Provider: provider, AuthSubjectID: item.subject,
			}
		}),
		Consumers: monitorFilterOptionsFrom(consumers, func(item *monitorFolded) MonitorFilterOption {
			name := names.resolve(item.key)
			return MonitorFilterOption{Value: item.key, Label: name.label, Requests: item.agg.Requests, Kind: name.kind}
		}),
	}

	out.Latency = monitorLatencySection(filter, w)
	out.Heatmap = monitorSection("heatmap", empty.Heatmap, func() (MonitorHeatmap, error) {
		return monitorHeatmap(w, filter)
	})
	out.Flows = monitorSection("flows", empty.Flows, func() (MonitorFlowGraph, error) {
		return monitorFlows(w, filter, names.label)
	})
	out.RecentFailures = monitorSection("recent failures", empty.RecentFailures, func() ([]MonitorFailure, error) {
		return monitorRecentFailures(filter, w.start, w.end, names.label)
	})
	return out, nil
}

// QueryMonitorRealtime returns the last hour at one-minute resolution.
func QueryMonitorRealtime(filter MonitorFilter, now time.Time) (MonitorRealtime, error) {
	filter = filter.normalized()
	w := resolveMonitorWindow(MonitorRange1h, now, getUsageLocation())
	out := MonitorRealtime{
		GeneratedAt: now.In(w.loc).Format(time.RFC3339),
		Timezone:    w.loc.String(),
	}
	aggs := make([]monitorAgg, w.points())
	if getReadDB() != nil {
		var err error
		if aggs, _, err = monitorSeriesAggs(w, filter); err != nil {
			return out, err
		}
	}
	out.Points = monitorSeriesPoints(w, aggs)
	n := len(aggs)
	if n == 0 {
		return out, nil
	}
	// The last point is the minute in progress: it feeds peaks and the
	// five-minute window, but rates read the last completed minute.
	if n >= 2 {
		out.CurrentRPM = aggs[n-2].Requests
		out.CurrentTPM = aggs[n-2].TotalTokens
	}
	var completed monitorAgg
	for i, agg := range aggs {
		out.PeakRPM = max(out.PeakRPM, agg.Requests)
		out.PeakTPM = max(out.PeakTPM, agg.TotalTokens)
		if i < n-1 {
			completed.add(agg)
		}
	}
	if n > 1 {
		out.AvgRPM = float64(completed.Requests) / float64(n-1)
		out.AvgTPM = float64(completed.TotalTokens) / float64(n-1)
	}
	var last5m monitorAgg
	for _, agg := range aggs[max(0, n-5):] {
		last5m.add(agg)
	}
	out.Last5m = last5m.totals()
	return out, nil
}

func monitorLatencySection(filter MonitorFilter, w monitorWindow) MonitorLatency {
	latency := emptyMonitorOverview(w).Latency
	total, oldestTotal, err := monitorLatencyHistogram(filter, w.start, w.end, "latency_ms")
	if err != nil {
		log.Warnf("usage: monitor latency section degraded: %v", err)
		return latency
	}
	firstToken, oldestFirst, err := monitorLatencyHistogram(filter, w.start, w.end, "first_token_ms")
	if err != nil {
		log.Warnf("usage: monitor first-token section degraded: %v", err)
	}
	latency.Total = total
	if err == nil {
		latency.FirstToken = firstToken
	}
	oldest := oldestTotal
	if !oldestFirst.IsZero() && (oldest.IsZero() || oldestFirst.Before(oldest)) {
		oldest = oldestFirst
	}
	if !oldest.IsZero() {
		latency.CoverageStart = oldest.In(w.loc).Format(time.RFC3339)
	}
	return latency
}

// monitorSection runs a secondary section; a failure is logged and replaced by
// fallback (an empty, non-nil value) so the payload keeps its full shape.
func monitorSection[T any](name string, fallback T, run func() (T, error)) T {
	value, err := run()
	if err != nil {
		log.Warnf("usage: monitor %s section degraded: %v", name, err)
		return fallback
	}
	return value
}

func emptyMonitorLatencyStats() MonitorLatencyStats {
	return MonitorLatencyStats{Histogram: make([]int64, len(monitorLatencyBoundsMs)+1)}
}

func emptyMonitorOverview(w monitorWindow) MonitorOverview {
	heatStart, _, heatDays := w.heatmapWindow()
	return MonitorOverview{
		GeneratedAt: w.now.Format(time.RFC3339),
		Range:       w.describe(),
		Series:      monitorSeriesPoints(w, make([]monitorAgg, w.points())),
		Latency: MonitorLatency{
			BoundsMs:   monitorLatencyBoundsMs,
			Total:      emptyMonitorLatencyStats(),
			FirstToken: emptyMonitorLatencyStats(),
		},
		Models:         MonitorBreakdown{Rows: []MonitorBreakdownRow{}},
		Channels:       MonitorBreakdown{Rows: []MonitorBreakdownRow{}},
		Consumers:      MonitorBreakdown{Rows: []MonitorBreakdownRow{}},
		Heatmap:        MonitorHeatmap{Start: heatStart.Format(time.RFC3339), Days: heatDays, Cells: []MonitorHeatmapCell{}},
		Flows:          MonitorFlowGraph{Nodes: []MonitorFlowNode{}, Links: []MonitorFlowLink{}},
		RecentFailures: []MonitorFailure{},
		Filters: MonitorFilterOptions{
			Models:    []MonitorFilterOption{},
			Channels:  []MonitorFilterOption{},
			Consumers: []MonitorFilterOption{},
		},
	}
}

type monitorConsumerName struct {
	label string
	kind  string
	hint  string
}

// monitorConsumerNames resolves consumer selectors to display names once per
// request: end users by display name, standalone keys by their own name with
// a masked hint. Deleted keys or users resolve to an empty label and the panel
// says so; the raw secret is never part of the payload.
type monitorConsumerNames struct {
	tenantID string
	rows     map[string]APIKeyRow
	cache    map[string]monitorConsumerName
}

func newMonitorConsumerNames(tenantID string) *monitorConsumerNames {
	return &monitorConsumerNames{tenantID: tenantID, cache: make(map[string]monitorConsumerName)}
}

func (n *monitorConsumerNames) resolve(key string) monitorConsumerName {
	if cached, ok := n.cache[key]; ok {
		return cached
	}
	var name monitorConsumerName
	switch {
	case strings.HasPrefix(key, monitorConsumerEndUserPrefix):
		name.kind = "end_user"
		name.label = DisplayNameForEndUser(strings.TrimPrefix(key, monitorConsumerEndUserPrefix))
	case strings.HasPrefix(key, monitorConsumerAPIKeyPrefix):
		name.kind = "api_key"
		if n.rows == nil {
			n.rows = currentAPIKeyRowsByIDForTenant(n.tenantID)
		}
		if row, ok := n.rows[strings.TrimPrefix(key, monitorConsumerAPIKeyPrefix)]; ok {
			name.label = strings.TrimSpace(row.Name)
			name.hint = monitorKeyHint(row.Key)
		}
	}
	n.cache[key] = name
	return name
}

func (n *monitorConsumerNames) label(key string) string {
	return n.resolve(key).label
}

// monitorKeyHint shows just enough of a key to recognise it (prefix and last
// four characters); short secrets show nothing.
func monitorKeyHint(secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) <= 12 {
		return ""
	}
	return secret[:3] + "…" + secret[len(secret)-4:]
}
