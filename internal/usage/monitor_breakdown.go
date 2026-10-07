package usage

import (
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Rankings run in two stages so their cost does not grow with cardinality x
// buckets: stage one aggregates each dimension value over the whole window,
// stage two fetches per-bucket counts only for the top rows that show a
// sparkline.

const (
	monitorBreakdownLimit    = 50
	monitorTrendRows         = 10
	monitorFilterOptionLimit = 200
	monitorFlowLayerLimit    = 6
	monitorStageOneLimit     = 5000
	monitorFlowOtherKey      = "__other__"
	monitorFlowUnknownKey    = "__unknown__"
)

// monitorFolded is one dimension value after folding raw rollup rows.
type monitorFolded struct {
	key     string
	label   string
	subject string
	agg     monitorAgg
	// weights pick the spelling / subject that carries the most traffic.
	labelWeight   int64
	subjectWeight int64
}

type monitorDimension struct {
	groupCols []string
	fold      func(dims []string) (key, label, subject string)
	// trendCols group stage two; the last column is always bucket_start and
	// trendKey reads the dimension key from the columns before it.
	trendCols  []string
	trendKey   func(dims []string) string
	trendWhere func(keys []string) (string, []any)
}

var monitorModelDimension = monitorDimension{
	groupCols: []string{"model"},
	fold: func(dims []string) (string, string, string) {
		model := strings.TrimSpace(dims[0])
		return model, model, ""
	},
	trendCols: []string{"model", "bucket_start"},
	trendKey:  func(dims []string) string { return strings.TrimSpace(dims[0]) },
	trendWhere: func(keys []string) (string, []any) {
		return " AND model IN (" + placeholders(len(keys)) + ")", stringsToArgs(keys)
	},
}

// Channels fold case-insensitively, like the request log channel filter.
var monitorChannelDimension = monitorDimension{
	groupCols: []string{"channel_name", "auth_subject_id"},
	fold: func(dims []string) (string, string, string) {
		label := strings.TrimSpace(dims[0])
		return strings.ToLower(label), label, strings.TrimSpace(dims[1])
	},
	trendCols: []string{"lower(trim(channel_name))", "bucket_start"},
	trendKey:  func(dims []string) string { return strings.ToLower(strings.TrimSpace(dims[0])) },
	trendWhere: func(keys []string) (string, []any) {
		return " AND lower(trim(channel_name)) IN (" + placeholders(len(keys)) + ")", stringsToArgs(keys)
	},
}

var monitorConsumerDimension = monitorDimension{
	groupCols: []string{"end_user_id", "api_key_id"},
	fold: func(dims []string) (string, string, string) {
		return monitorConsumerKey(dims[0], dims[1]), "", ""
	},
	trendCols: []string{"end_user_id", "api_key_id", "bucket_start"},
	trendKey:  func(dims []string) string { return monitorConsumerKey(dims[0], dims[1]) },
	trendWhere: func(keys []string) (string, []any) {
		endUsers, apiKeys := ParseMonitorConsumers(keys)
		var clauses []string
		var args []any
		if len(endUsers) > 0 {
			clauses = append(clauses, "end_user_id IN ("+placeholders(len(endUsers))+")")
			args = append(args, stringsToArgs(endUsers)...)
		}
		if len(apiKeys) > 0 {
			// Owned keys fold into their end user, so a "key:" row only ever
			// stands for keys without an owner.
			clauses = append(clauses, "(end_user_id = '' AND api_key_id IN ("+placeholders(len(apiKeys))+"))")
			args = append(args, stringsToArgs(apiKeys)...)
		}
		if len(clauses) == 0 {
			return " AND 1 = 0", nil
		}
		return " AND (" + strings.Join(clauses, " OR ") + ")", args
	},
}

func stringsToArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func foldMonitorRows(rows []monitorRollupRow, fold func([]string) (string, string, string)) []*monitorFolded {
	byKey := make(map[string]*monitorFolded, len(rows))
	order := make([]*monitorFolded, 0, len(rows))
	for _, row := range rows {
		key, label, subject := fold(row.dims)
		folded := byKey[key]
		if folded == nil {
			folded = &monitorFolded{key: key}
			byKey[key] = folded
			order = append(order, folded)
		}
		folded.agg.add(row.agg)
		if label != "" && (folded.label == "" || row.agg.Requests > folded.labelWeight) {
			folded.label, folded.labelWeight = label, row.agg.Requests
		}
		if subject != "" && (folded.subject == "" || row.agg.Requests > folded.subjectWeight) {
			folded.subject, folded.subjectWeight = subject, row.agg.Requests
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].agg.Requests != order[j].agg.Requests {
			return order[i].agg.Requests > order[j].agg.Requests
		}
		return order[i].key < order[j].key
	})
	return order
}

// monitorStageOne aggregates one dimension over the window, heaviest first.
func monitorStageOne(w monitorWindow, filter MonitorFilter, dim monitorDimension) ([]*monitorFolded, error) {
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: w.bucketKind, from: w.start, to: w.end, loc: w.loc,
		groupCols: dim.groupCols, limit: monitorStageOneLimit,
	})
	if err != nil {
		return nil, err
	}
	return foldMonitorRows(rows, dim.fold), nil
}

// monitorBreakdownRows turns folded values into ranking rows, attaching the
// per-bucket trend to the first monitorTrendRows of them. A failed trend query
// only drops the sparklines; the ranking itself is already complete.
func monitorBreakdownRows(w monitorWindow, filter MonitorFilter, dim monitorDimension, folded []*monitorFolded) MonitorBreakdown {
	out := MonitorBreakdown{Rows: []MonitorBreakdownRow{}, Total: len(folded)}
	limit := min(len(folded), monitorBreakdownLimit)
	index := make(map[string]int, limit)
	trendKeys := make([]string, 0, monitorTrendRows)
	for i := range limit {
		item := folded[i]
		out.Rows = append(out.Rows, MonitorBreakdownRow{
			Key:           item.key,
			Label:         item.label,
			AuthSubjectID: item.subject,
			MonitorTotals: item.agg.totals(),
		})
		index[item.key] = i
		if i < monitorTrendRows && item.key != "" {
			trendKeys = append(trendKeys, item.key)
		}
	}
	if len(trendKeys) == 0 {
		return out
	}
	where, args := dim.trendWhere(trendKeys)
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: w.bucketKind, from: w.start, to: w.end, loc: w.loc,
		groupCols: dim.trendCols, extraWhere: where, extraArgs: args,
	})
	if err != nil {
		log.Warnf("usage: monitor ranking trend degraded: %v", err)
		return out
	}
	points := w.points()
	for _, trendKey := range trendKeys {
		row := &out.Rows[index[trendKey]]
		row.Trend = make([]int64, points)
		row.TrendFailed = make([]int64, points)
	}
	last := len(dim.trendCols) - 1
	for _, raw := range rows {
		i, ok := index[dim.trendKey(raw.dims[:last])]
		if !ok || out.Rows[i].Trend == nil {
			continue
		}
		at, ok := parseRollupKey(w.bucketKind, raw.dims[last], w.loc)
		if !ok {
			continue
		}
		if bucket := w.seriesIndex(at); bucket >= 0 {
			out.Rows[i].Trend[bucket] += raw.agg.Requests
			out.Rows[i].TrendFailed[bucket] += raw.agg.Failed
		}
	}
	return out
}

func monitorFilterOptionsFrom(folded []*monitorFolded, describe func(*monitorFolded) MonitorFilterOption) []MonitorFilterOption {
	options := make([]MonitorFilterOption, 0, min(len(folded), monitorFilterOptionLimit))
	for _, item := range folded {
		if item.key == "" {
			// "Unknown channel" / "no key" rows cannot be selected as a filter.
			continue
		}
		options = append(options, describe(item))
		if len(options) == monitorFilterOptionLimit {
			break
		}
	}
	return options
}

// monitorFlows builds the consumer -> model -> channel Sankey graph. Each
// layer keeps its heaviest values and folds the rest into one "other" node, so
// the graph stays readable and flow is conserved through the model layer.
func monitorFlows(w monitorWindow, filter MonitorFilter, consumerLabel func(string) string) (MonitorFlowGraph, error) {
	graph := MonitorFlowGraph{Nodes: []MonitorFlowNode{}, Links: []MonitorFlowLink{}}
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: w.bucketKind, from: w.start, to: w.end, loc: w.loc,
		groupCols: []string{"end_user_id", "api_key_id", "model", "channel_name"},
		limit:     monitorStageOneLimit,
	})
	if err != nil {
		return graph, err
	}
	type flow struct {
		consumer, model, channel string
		agg                      monitorAgg
	}
	flows := make([]flow, 0, len(rows))
	channelLabels := make(map[string]string)
	layerTotals := [3]map[string]int64{{}, {}, {}}
	for _, row := range rows {
		consumer := monitorConsumerKey(row.dims[0], row.dims[1])
		model := strings.TrimSpace(row.dims[2])
		channelLabel := strings.TrimSpace(row.dims[3])
		channel := strings.ToLower(channelLabel)
		if _, ok := channelLabels[channel]; !ok {
			channelLabels[channel] = channelLabel
		}
		flows = append(flows, flow{consumer: consumer, model: model, channel: channel, agg: row.agg})
		layerTotals[0][consumer] += row.agg.Requests
		layerTotals[1][model] += row.agg.Requests
		layerTotals[2][channel] += row.agg.Requests
	}
	keep := [3]map[string]bool{}
	for layer := range 3 {
		keep[layer] = topMonitorKeys(layerTotals[layer], monitorFlowLayerLimit)
	}
	collapse := func(layer int, key string) string {
		switch {
		case key == "":
			return monitorFlowUnknownKey
		case keep[layer][key]:
			return key
		default:
			return monitorFlowOtherKey
		}
	}
	type edge struct{ source, target string }
	edges := make(map[edge]*MonitorFlowLink)
	nodes := make(map[string]MonitorFlowNode)
	addNode := func(layer, key, label string) string {
		id := layer + ":" + key
		if _, ok := nodes[id]; !ok {
			if key == monitorFlowOtherKey || key == monitorFlowUnknownKey {
				label = ""
			}
			nodes[id] = MonitorFlowNode{ID: id, Label: label, Layer: layer}
		}
		return id
	}
	addEdge := func(source, target string, agg monitorAgg) {
		link := edges[edge{source, target}]
		if link == nil {
			link = &MonitorFlowLink{Source: source, Target: target}
			edges[edge{source, target}] = link
		}
		link.Requests += agg.Requests
		link.Tokens += agg.TotalTokens
	}
	for _, f := range flows {
		if f.agg.Requests == 0 {
			continue
		}
		consumer := collapse(0, f.consumer)
		model := collapse(1, f.model)
		channel := collapse(2, f.channel)
		consumerID := addNode("consumer", consumer, consumerLabel(consumer))
		modelID := addNode("model", model, model)
		channelID := addNode("channel", channel, channelLabels[channel])
		addEdge(consumerID, modelID, f.agg)
		addEdge(modelID, channelID, f.agg)
	}
	for _, node := range nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].ID < graph.Nodes[j].ID })
	for _, link := range edges {
		graph.Links = append(graph.Links, *link)
	}
	sort.Slice(graph.Links, func(i, j int) bool {
		if graph.Links[i].Requests != graph.Links[j].Requests {
			return graph.Links[i].Requests > graph.Links[j].Requests
		}
		if graph.Links[i].Source != graph.Links[j].Source {
			return graph.Links[i].Source < graph.Links[j].Source
		}
		return graph.Links[i].Target < graph.Links[j].Target
	})
	return graph, nil
}

func topMonitorKeys(totals map[string]int64, limit int) map[string]bool {
	keys := make([]string, 0, len(totals))
	for key := range totals {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	keep := make(map[string]bool, limit)
	for _, key := range keys[:min(len(keys), limit)] {
		keep[key] = true
	}
	return keep
}
