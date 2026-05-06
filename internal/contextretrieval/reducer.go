package contextretrieval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/sjson"
	_ "modernc.org/sqlite"
)

type Report struct {
	Applied       bool
	Field         string
	OriginalBytes int
	ReducedBytes  int
	OriginalItems int
	KeptItems     int
	MatchedItems  int
}

type item struct {
	Index  int
	Raw    json.RawMessage
	Role   string
	Text   string
	Recent bool
	Forced bool
}

var keywordPattern = regexp.MustCompile(`[A-Za-z0-9_]{3,}`)
var filePathPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|md|yaml|yml|json|toml|sh|sql|css|html)`)

const (
	toolPairRepairOff         = "off"
	toolPairRepairDropOrphans = "drop-orphans"
)

func Reduce(ctx context.Context, raw []byte, model, protocol string, cfg config.ContextRetrievalConfig) ([]byte, Report, error) {
	_ = ctx
	report := Report{OriginalBytes: len(raw)}
	if !cfg.Enabled || len(raw) == 0 {
		return raw, report, nil
	}
	normalizeConfig(&cfg)
	if cfg.MaxInputBytes <= 0 {
		return raw, report, nil
	}
	overBudget := len(raw) > cfg.MaxInputBytes
	repairMode := ""
	if cfg.CodexAware.Enabled {
		repairMode = cfg.CodexAware.ToolPairRepair
	}
	if !overBudget && (repairMode == "" || repairMode == toolPairRepairOff) {
		return raw, report, nil
	}
	if !modelAllowed(cfg.Models, model, protocol) {
		return raw, report, nil
	}

	field, items, err := extractItems(raw)
	if err != nil || len(items) == 0 {
		return raw, report, err
	}
	report.Field = field
	report.OriginalItems = len(items)

	if !overBudget {
		keep := make(map[int]struct{}, len(items))
		for i := range items {
			keep[items[i].Index] = struct{}{}
		}
		if !repairToolPairKeep(items, keep, repairMode) {
			return raw, report, nil
		}
		repairOnlyCfg := cfg
		repairOnlyCfg.CodexAware.InsertSummary = false
		repaired, err := assemble(raw, field, items, keep, repairOnlyCfg)
		if err != nil {
			return raw, report, err
		}
		report.Applied = true
		report.ReducedBytes = len(repaired)
		report.KeptItems = len(keep)
		return repaired, report, nil
	}

	markPreserved(items, cfg.PreserveRecentTurns, cfg.CodexAware)
	query := buildQuery(items)
	matched := map[int]struct{}{}
	if query != "" {
		var errSearch error
		matched, errSearch = searchItems(items, query, cfg.Chunk.MaxBytes, cfg.Retrieval.TopK)
		if errSearch != nil {
			return raw, report, errSearch
		}
	}
	report.MatchedItems = len(matched)

	keep := make(map[int]struct{}, len(items))
	for i := range items {
		if items[i].Recent || items[i].Forced {
			keep[items[i].Index] = struct{}{}
		}
	}
	for idx := range matched {
		keep[idx] = struct{}{}
	}
	if cfg.CodexAware.Enabled && cfg.CodexAware.PreserveToolPairs {
		preserveToolPairs(items, keep)
	}
	reduced, kept, err := assembleWithinBudget(raw, field, items, keep, cfg.MaxInputBytes, cfg)
	if err != nil {
		return raw, report, err
	}
	if len(reduced) >= len(raw) {
		return raw, report, nil
	}
	report.Applied = true
	report.ReducedBytes = len(reduced)
	report.KeptItems = kept
	return reduced, report, nil
}

func normalizeConfig(cfg *config.ContextRetrievalConfig) {
	if cfg.MaxInputBytes <= 0 {
		cfg.MaxInputBytes = 700000
	}
	if cfg.PreserveRecentTurns <= 0 {
		cfg.PreserveRecentTurns = 6
	}
	if cfg.Chunk.MaxBytes <= 0 {
		cfg.Chunk.MaxBytes = 12000
	}
	if cfg.Retrieval.TopK <= 0 {
		cfg.Retrieval.TopK = 20
	}
	if cfg.CodexAware.Enabled {
		cfg.CodexAware.ToolPairRepair = strings.ToLower(strings.TrimSpace(cfg.CodexAware.ToolPairRepair))
		if cfg.CodexAware.ToolPairRepair == "" && cfg.CodexAware.PreserveToolPairs {
			cfg.CodexAware.ToolPairRepair = toolPairRepairDropOrphans
		}
		if cfg.CodexAware.MaxSummaryBytes <= 0 {
			cfg.CodexAware.MaxSummaryBytes = 4000
		}
		if cfg.CodexAware.PreserveRecentCommands <= 0 {
			cfg.CodexAware.PreserveRecentCommands = 8
		}
		if cfg.CodexAware.PreserveRecentErrors <= 0 {
			cfg.CodexAware.PreserveRecentErrors = 8
		}
	}
}

func extractItems(raw []byte) (string, []item, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", nil, err
	}
	for _, field := range []string{"input", "messages"} {
		rawItems, ok := root[field]
		if !ok || len(rawItems) == 0 || string(rawItems) == "null" {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(rawItems, &arr); err != nil {
			continue
		}
		items := make([]item, 0, len(arr))
		for i := range arr {
			items = append(items, item{
				Index: i,
				Raw:   arr[i],
				Role:  itemRole(arr[i]),
				Text:  extractText(arr[i]),
			})
		}
		return field, items, nil
	}
	return "", nil, nil
}

func itemRole(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var role string
	_ = json.Unmarshal(obj["role"], &role)
	return strings.ToLower(strings.TrimSpace(role))
}

func extractText(raw json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return string(raw)
	}
	var parts []string
	walkText(value, &parts)
	return strings.Join(parts, "\n")
}

func walkText(value any, parts *[]string) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			*parts = append(*parts, v)
		}
	case []any:
		for _, item := range v {
			walkText(item, parts)
		}
	case map[string]any:
		for key, item := range v {
			lower := strings.ToLower(strings.TrimSpace(key))
			if strings.Contains(lower, "image") || strings.Contains(lower, "base64") {
				continue
			}
			walkText(item, parts)
		}
	}
}

func markPreserved(items []item, recent int, codex config.CodexAwareContextConfig) {
	if recent <= 0 {
		recent = 6
	}
	start := len(items) - recent
	if start < 0 {
		start = 0
	}
	for i := range items {
		if i >= start {
			items[i].Recent = true
		}
		switch items[i].Role {
		case "system", "developer":
			items[i].Forced = true
		}
	}
	if !codex.Enabled {
		return
	}
	preserveMatchingRecentSignals(items, commandLike, codex.PreserveRecentCommands)
	preserveMatchingRecentSignals(items, errorLike, codex.PreserveRecentErrors)
}

func preserveMatchingRecentSignals(items []item, match func(string) bool, limit int) {
	if limit <= 0 {
		return
	}
	for i := len(items) - 1; i >= 0 && limit > 0; i-- {
		if items[i].Forced || items[i].Recent {
			continue
		}
		if match(items[i].Text) {
			items[i].Forced = true
			limit--
		}
	}
}

func commandLike(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "go test ") || strings.HasPrefix(line, "git ") || strings.HasPrefix(line, "rg ") || strings.Contains(line, `"cmd"`) {
			return true
		}
	}
	return false
}

func errorLike(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "panic") || strings.Contains(lower, "exception") || strings.Contains(lower, "timeout") || strings.Contains(lower, "traceback") || strings.Contains(lower, " 503") || strings.Contains(lower, " 429") || strings.Contains(lower, " 404")
}

func buildQuery(items []item) string {
	var seed strings.Builder
	for i := len(items) - 1; i >= 0 && seed.Len() < 12000; i-- {
		if items[i].Recent || items[i].Forced {
			seed.WriteByte('\n')
			seed.WriteString(items[i].Text)
		}
	}
	matches := keywordPattern.FindAllString(seed.String(), -1)
	seen := make(map[string]struct{}, len(matches))
	terms := make([]string, 0, 32)
	for _, term := range matches {
		term = strings.ToLower(strings.TrimSpace(term))
		if len(term) < 3 {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		if len(terms) >= 32 {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

func searchItems(items []item, query string, maxChunkBytes int, topK int) (map[int]struct{}, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE VIRTUAL TABLE chunks USING fts5(item_idx UNINDEXED, body)`); err != nil {
		return nil, err
	}
	stmt, err := db.Prepare(`INSERT INTO chunks(item_idx, body) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i := range items {
		if items[i].Recent || items[i].Forced || strings.TrimSpace(items[i].Text) == "" {
			continue
		}
		for _, chunk := range splitByBytes(items[i].Text, maxChunkBytes) {
			if _, err = stmt.Exec(items[i].Index, chunk); err != nil {
				return nil, err
			}
		}
	}
	limit := topK * 4
	if limit <= 0 {
		limit = 80
	}
	rows, err := db.Query(`SELECT item_idx FROM chunks WHERE chunks MATCH ? ORDER BY bm25(chunks) LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]struct{}, topK)
	for rows.Next() {
		var idx int
		if err = rows.Scan(&idx); err != nil {
			return nil, err
		}
		out[idx] = struct{}{}
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out, rows.Err()
}

func splitByBytes(text string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = 12000
	}
	if len(text) <= maxBytes {
		return []string{text}
	}
	var chunks []string
	var builder strings.Builder
	for _, r := range text {
		if builder.Len()+len(string(r)) > maxBytes && builder.Len() > 0 {
			chunks = append(chunks, builder.String())
			builder.Reset()
		}
		builder.WriteRune(r)
	}
	if builder.Len() > 0 {
		chunks = append(chunks, builder.String())
	}
	return chunks
}

func assembleWithinBudget(raw []byte, field string, items []item, keep map[int]struct{}, maxBytes int, cfg config.ContextRetrievalConfig) ([]byte, int, error) {
	if len(keep) == 0 {
		return raw, 0, nil
	}
	toolGroups := toolPairGroupsByIndex(items)
	toolRepair := ""
	if cfg.CodexAware.Enabled {
		toolRepair = cfg.CodexAware.ToolPairRepair
	}
	repairToolPairKeep(items, keep, toolRepair)
	reduced, err := assemble(raw, field, items, keep, cfg)
	if err != nil || len(reduced) <= maxBytes {
		return reduced, len(keep), err
	}

	removable := make([]int, 0, len(keep))
	for i := range items {
		if _, ok := keep[items[i].Index]; !ok {
			continue
		}
		if items[i].Forced || items[i].Recent {
			continue
		}
		removable = append(removable, items[i].Index)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(removable)))
	for _, idx := range removable {
		if !deleteKeepItem(keep, idx, toolGroups, protectedToolPairIndexes(items, keep, func(it item) bool {
			return it.Forced || it.Recent
		})) {
			continue
		}
		repairToolPairKeep(items, keep, toolRepair)
		reduced, err = assemble(raw, field, items, keep, cfg)
		if err != nil || len(reduced) <= maxBytes {
			return reduced, len(keep), err
		}
	}

	recentRemovable := make([]int, 0, len(keep))
	for i := range items {
		if _, ok := keep[items[i].Index]; !ok {
			continue
		}
		if items[i].Forced || items[i].Index == len(items)-1 {
			continue
		}
		recentRemovable = append(recentRemovable, items[i].Index)
	}
	sort.Ints(recentRemovable)
	for _, idx := range recentRemovable {
		if !deleteKeepItem(keep, idx, toolGroups, protectedToolPairIndexes(items, keep, func(it item) bool {
			return it.Forced || it.Index == len(items)-1
		})) {
			continue
		}
		repairToolPairKeep(items, keep, toolRepair)
		reduced, err = assemble(raw, field, items, keep, cfg)
		if err != nil || len(reduced) <= maxBytes {
			return reduced, len(keep), err
		}
	}
	return reduced, len(keep), err
}

func deleteKeepItem(keep map[int]struct{}, idx int, groups map[int][]int, protected map[int]struct{}) bool {
	toDelete := []int{idx}
	if group := groups[idx]; len(group) > 0 {
		toDelete = group
	}
	for _, candidate := range toDelete {
		if _, ok := keep[candidate]; !ok {
			continue
		}
		if _, ok := protected[candidate]; ok {
			return false
		}
	}
	for _, candidate := range toDelete {
		delete(keep, candidate)
	}
	return true
}

func protectedToolPairIndexes(items []item, keep map[int]struct{}, protected func(item) bool) map[int]struct{} {
	out := map[int]struct{}{}
	for i := range items {
		if _, ok := keep[items[i].Index]; !ok {
			continue
		}
		if protected(items[i]) {
			out[items[i].Index] = struct{}{}
		}
	}
	return out
}

func assemble(raw []byte, field string, items []item, keep map[int]struct{}, cfg config.ContextRetrievalConfig) ([]byte, error) {
	selected := make([]json.RawMessage, 0, len(keep))
	for i := range items {
		if _, ok := keep[items[i].Index]; ok {
			selected = append(selected, items[i].Raw)
		}
	}
	if cfg.CodexAware.Enabled && cfg.CodexAware.InsertSummary {
		if summary := buildCodexSummary(items, keep, cfg.CodexAware.MaxSummaryBytes); summary != "" {
			selected = insertSummaryItem(field, selected, summary)
		}
	}
	arr, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	out, err := sjson.SetRawBytes(raw, field, arr)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func preserveToolPairs(items []item, keep map[int]struct{}) {
	if len(keep) == 0 {
		return
	}
	callIDs := map[string][]int{}
	for i := range items {
		for _, id := range itemCallIDs(items[i].Raw) {
			callIDs[id] = append(callIDs[id], items[i].Index)
		}
	}
	for _, idxs := range callIDs {
		shouldKeep := false
		for _, idx := range idxs {
			if _, ok := keep[idx]; ok {
				shouldKeep = true
				break
			}
		}
		if shouldKeep {
			for _, idx := range idxs {
				keep[idx] = struct{}{}
			}
		}
	}
}

func toolPairGroupsByIndex(items []item) map[int][]int {
	byCallID := map[string][]int{}
	for i := range items {
		for _, id := range itemCallIDs(items[i].Raw) {
			byCallID[id] = append(byCallID[id], items[i].Index)
		}
	}
	out := map[int][]int{}
	for _, idxs := range byCallID {
		if len(idxs) < 2 {
			continue
		}
		for _, idx := range idxs {
			out[idx] = idxs
		}
	}
	return out
}

func repairToolPairKeep(items []item, keep map[int]struct{}, mode string) bool {
	if mode == "" || mode == toolPairRepairOff || len(keep) == 0 {
		return false
	}
	if mode != toolPairRepairDropOrphans {
		return false
	}

	changed := false
	type pairState struct {
		calls   []int
		outputs []int
	}
	states := map[string]*pairState{}
	for i := range items {
		if _, ok := keep[items[i].Index]; !ok {
			continue
		}
		kind, ids := toolPairItemKind(items[i].Raw)
		if kind == "" || len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			state := states[id]
			if state == nil {
				state = &pairState{}
				states[id] = state
			}
			switch kind {
			case "call":
				state.calls = append(state.calls, items[i].Index)
			case "output":
				state.outputs = append(state.outputs, items[i].Index)
			}
		}
	}
	for _, state := range states {
		if len(state.calls) == 0 || len(state.outputs) > 0 {
			continue
		}
		for _, idx := range state.calls {
			if _, ok := keep[idx]; ok {
				delete(keep, idx)
				changed = true
			}
		}
	}
	return changed
}

func toolPairItemKind(raw json.RawMessage) (string, []string) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", nil
	}
	itemType, _ := obj["type"].(string)
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	switch itemType {
	case "function_call":
		return "call", itemCallIDs(raw)
	case "function_call_output":
		return "output", itemCallIDs(raw)
	default:
		return "", nil
	}
}

func itemCallIDs(raw json.RawMessage) []string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, key := range []string{"call_id", "tool_call_id"} {
				if s, ok := x[key].(string); ok && strings.TrimSpace(s) != "" {
					id := strings.TrimSpace(s)
					if _, exists := seen[id]; !exists {
						seen[id] = struct{}{}
						out = append(out, id)
					}
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	return out
}

func buildCodexSummary(items []item, keep map[int]struct{}, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 4000
	}
	paths := orderedUniqueMatches(items, keep, filePathPattern, 24)
	errors := collectLines(items, keep, errorLike, 8)
	commands := collectLines(items, keep, commandLike, 8)
	if len(paths) == 0 && len(errors) == 0 && len(commands) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Retrieved summary from older omitted Codex context. Prefer recent explicit messages over this summary.\n")
	if len(paths) > 0 {
		b.WriteString("Relevant file paths: ")
		b.WriteString(strings.Join(paths, ", "))
		b.WriteByte('\n')
	}
	if len(commands) > 0 {
		b.WriteString("Recent commands/tool activity:\n")
		for _, line := range commands {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if len(errors) > 0 {
		b.WriteString("Recent errors/signals:\n")
		for _, line := range errors {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	out := b.String()
	if len(out) > maxBytes {
		out = out[:maxBytes]
	}
	return out
}

func orderedUniqueMatches(items []item, keep map[int]struct{}, re *regexp.Regexp, limit int) []string {
	seen := map[string]struct{}{}
	var out []string
	for i := len(items) - 1; i >= 0 && len(out) < limit; i-- {
		if _, ok := keep[items[i].Index]; ok {
			continue
		}
		for _, match := range re.FindAllString(items[i].Text, -1) {
			match = strings.TrimSpace(match)
			if match == "" {
				continue
			}
			key := strings.ToLower(match)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, match)
			if len(out) >= limit {
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func collectLines(items []item, keep map[int]struct{}, match func(string) bool, limit int) []string {
	var out []string
	seen := map[string]struct{}{}
	for i := len(items) - 1; i >= 0 && len(out) < limit; i-- {
		if _, ok := keep[items[i].Index]; ok {
			continue
		}
		for _, line := range strings.Split(items[i].Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !match(line) {
				continue
			}
			if len(line) > 240 {
				line = line[:240]
			}
			key := strings.ToLower(line)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, line)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func insertSummaryItem(field string, selected []json.RawMessage, summary string) []json.RawMessage {
	var raw []byte
	if field == "messages" {
		raw, _ = json.Marshal(map[string]any{"role": "system", "content": summary})
	} else {
		raw, _ = json.Marshal(map[string]any{
			"type":    "message",
			"role":    "developer",
			"content": []map[string]string{{"type": "input_text", "text": summary}},
		})
	}
	if len(raw) == 0 {
		return selected
	}
	insertAt := 0
	for insertAt < len(selected) {
		role := itemRole(selected[insertAt])
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	out := make([]json.RawMessage, 0, len(selected)+1)
	out = append(out, selected[:insertAt]...)
	out = append(out, json.RawMessage(raw))
	out = append(out, selected[insertAt:]...)
	return out
}

func modelAllowed(rules []config.PayloadModelRule, model, protocol string) bool {
	if len(rules) == 0 {
		return true
	}
	model = canonicalModel(model)
	for _, rule := range rules {
		if strings.TrimSpace(rule.Protocol) != "" && strings.TrimSpace(protocol) != "" && !strings.EqualFold(rule.Protocol, protocol) {
			continue
		}
		if globMatch(strings.TrimSpace(rule.Name), model) {
			return true
		}
	}
	return false
}

func canonicalModel(model string) string {
	model = strings.TrimSpace(model)
	parsed := thinking.ParseSuffix(model)
	if strings.TrimSpace(parsed.ModelName) != "" {
		model = parsed.ModelName
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func globMatch(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	pos := len(parts[0])
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	last := parts[len(parts)-1]
	return last == "" || strings.HasSuffix(value, last)
}

func (r Report) String() string {
	if !r.Applied {
		return "context retrieval not applied"
	}
	return fmt.Sprintf("field=%s bytes=%d->%d items=%d->%d matched=%d", r.Field, r.OriginalBytes, r.ReducedBytes, r.OriginalItems, r.KeptItems, r.MatchedItems)
}
