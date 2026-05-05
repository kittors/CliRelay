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

func Reduce(ctx context.Context, raw []byte, model, protocol string, cfg config.ContextRetrievalConfig) ([]byte, Report, error) {
	_ = ctx
	report := Report{OriginalBytes: len(raw)}
	if !cfg.Enabled || len(raw) == 0 {
		return raw, report, nil
	}
	normalizeConfig(&cfg)
	if cfg.MaxInputBytes <= 0 || len(raw) <= cfg.MaxInputBytes {
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

	markPreserved(items, cfg.PreserveRecentTurns)
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
	reduced, kept, err := assembleWithinBudget(raw, field, items, keep, cfg.MaxInputBytes)
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

func markPreserved(items []item, recent int) {
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

func assembleWithinBudget(raw []byte, field string, items []item, keep map[int]struct{}, maxBytes int) ([]byte, int, error) {
	if len(keep) == 0 {
		return raw, 0, nil
	}
	reduced, err := assemble(raw, field, items, keep)
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
		delete(keep, idx)
		reduced, err = assemble(raw, field, items, keep)
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
		delete(keep, idx)
		reduced, err = assemble(raw, field, items, keep)
		if err != nil || len(reduced) <= maxBytes {
			return reduced, len(keep), err
		}
	}
	return reduced, len(keep), err
}

func assemble(raw []byte, field string, items []item, keep map[int]struct{}) ([]byte, error) {
	selected := make([]json.RawMessage, 0, len(keep))
	for i := range items {
		if _, ok := keep[items[i].Index]; ok {
			selected = append(selected, items[i].Raw)
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
