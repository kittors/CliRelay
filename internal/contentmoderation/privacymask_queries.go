package contentmoderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What the panel reads of the records: the summary of the last days, the
// records a page at a time, one record's detail. Every query is of one tenant,
// and of one portal user when MaskScope says so: a portal user sees their own
// records only.

// MaskScope is whose records a query reads.
type MaskScope struct {
	TenantID string
	// EndUserID limits the query to one portal user's records.
	EndUserID string
	// APIKeyID limits it to one API key's.
	APIKeyID string
}

func (s MaskScope) where(args *[]any, prefix string) string {
	*args = append(*args, s.TenantID)
	clause := prefix + "tenant_id=$" + strconv.Itoa(len(*args))
	if s.EndUserID != "" {
		*args = append(*args, s.EndUserID)
		clause += " AND " + prefix + "end_user_id=$" + strconv.Itoa(len(*args))
	}
	if s.APIKeyID != "" {
		*args = append(*args, s.APIKeyID)
		clause += " AND " + prefix + "api_key_id=$" + strconv.Itoa(len(*args))
	}
	return clause
}

// MaskDay is one day of the summary.
type MaskDay struct {
	Day      string `json:"day"`
	Places   int64  `json:"places"`
	Requests int64  `json:"requests"`
}

// MaskCount is a client or a channel of the summary.
type MaskCount struct {
	Name     string `json:"name"`
	Places   int64  `json:"places"`
	Requests int64  `json:"requests"`
	Vendor   string `json:"vendor,omitempty"`
}

// MaskKindCount is a kind of the summary.
type MaskKindCount struct {
	Kind     string `json:"kind"`
	Places   int64  `json:"places"`
	Distinct int64  `json:"distinct"`
}

// MaskTopValue is one value across the requests of the summary.
type MaskTopValue struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	By       string `json:"by"`
	Head     string `json:"head,omitempty"`
	Len      int    `json:"len"`
	Places   int64  `json:"places"`
	Requests int64  `json:"requests"`
	LastAt   string `json:"last_at"`
}

// MaskSummary is the summary of the last days.
type MaskSummary struct {
	Days     []MaskDay       `json:"days"`
	Places   int64           `json:"places"`
	Distinct int64           `json:"distinct"`
	Requests int64           `json:"requests"`
	Blocked  int64           `json:"blocked"`
	Warned   int64           `json:"warned"`
	Kinds    []MaskKindCount `json:"kinds"`
	Clients  []MaskCount     `json:"clients"`
	Channels []MaskCount     `json:"channels"`
	Values   []MaskTopValue  `json:"values"`
}

// maxMaskTopValues bounds the values of a summary.
const maxMaskTopValues = 100

// firstMaskDay is the first of the last days days, today included.
func firstMaskDay(now time.Time, days int) string {
	return maskDay(now.In(time.Local).AddDate(0, 0, -(days - 1)))
}

// MaskSummary sums the last days days (today included) of scope.
func (s *Store) MaskSummary(ctx context.Context, scope MaskScope, days int, now time.Time) (MaskSummary, error) {
	if s == nil || s.db == nil {
		return MaskSummary{}, ErrUnavailable
	}
	from := firstMaskDay(now, days)
	out := MaskSummary{Days: []MaskDay{}, Kinds: []MaskKindCount{}, Clients: []MaskCount{}, Channels: []MaskCount{}, Values: []MaskTopValue{}}
	byDay := map[string]*MaskDay{}
	for d := 0; d < days; d++ {
		day := maskDay(now.In(time.Local).AddDate(0, 0, -(days - 1 - d)))
		out.Days = append(out.Days, MaskDay{Day: day})
	}
	for i := range out.Days {
		byDay[out.Days[i].Day] = &out.Days[i]
	}
	args := []any{}
	where := scope.where(&args, "") + " AND day>=$" + strconv.Itoa(len(args)+1)
	args = append(args, from)
	clients, channels := map[string]*MaskCount{}, map[string]*MaskCount{}
	err := s.eachRow(ctx, `SELECT day, client, channel, provider, requests, places, blocked, warned FROM privacy_mask_days WHERE `+where, args, func(rows *sql.Rows) error {
		var day, client, channel, provider string
		var requests, places, blocked, warned int64
		if err := rows.Scan(&day, &client, &channel, &provider, &requests, &places, &blocked, &warned); err != nil {
			return err
		}
		if d := byDay[day]; d != nil {
			d.Places += places
			d.Requests += requests
		}
		out.Places += places
		out.Requests += requests
		out.Blocked += blocked
		out.Warned += warned
		addCount(clients, client, "", places, requests)
		if channel != "" {
			addCount(channels, channel, provider, places, requests)
		}
		return nil
	})
	if err != nil {
		return MaskSummary{}, err
	}
	out.Clients, out.Channels = sortedCounts(clients), sortedCounts(channels)
	kinds := map[string]*MaskKindCount{}
	values := map[string]*MaskTopValue{}
	err = s.eachRow(ctx, `SELECT value_id, kind, finder, head, value_len, places, requests, last_ts FROM privacy_mask_value_days WHERE `+where, args, func(rows *sql.Rows) error {
		var v MaskTopValue
		if err := rows.Scan(&v.ID, &v.Kind, &v.By, &v.Head, &v.Len, &v.Places, &v.Requests, &v.LastAt); err != nil {
			return err
		}
		k := kinds[v.Kind]
		if k == nil {
			k = &MaskKindCount{Kind: v.Kind}
			kinds[v.Kind] = k
		}
		k.Places += v.Places
		have := values[v.ID]
		if have == nil {
			k.Distinct++
			values[v.ID] = &v
			return nil
		}
		have.Places += v.Places
		have.Requests += v.Requests
		if v.LastAt > have.LastAt {
			have.LastAt, have.Head, have.Len = v.LastAt, v.Head, v.Len
		}
		return nil
	})
	if err != nil {
		return MaskSummary{}, err
	}
	out.Distinct = int64(len(values))
	for _, k := range kinds {
		out.Kinds = append(out.Kinds, *k)
	}
	sortBy(out.Kinds, func(a, b MaskKindCount) bool { return a.Places > b.Places || a.Places == b.Places && a.Kind < b.Kind })
	for _, v := range values {
		out.Values = append(out.Values, *v)
	}
	sortBy(out.Values, func(a, b MaskTopValue) bool {
		return a.Places > b.Places || a.Places == b.Places && a.LastAt > b.LastAt
	})
	if len(out.Values) > maxMaskTopValues {
		out.Values = out.Values[:maxMaskTopValues]
	}
	return out, nil
}

func addCount(into map[string]*MaskCount, name, vendor string, places, requests int64) {
	c := into[name]
	if c == nil {
		c = &MaskCount{Name: name, Vendor: vendor}
		into[name] = c
	}
	c.Places += places
	c.Requests += requests
	if c.Vendor == "" {
		c.Vendor = vendor
	}
}

func sortedCounts(in map[string]*MaskCount) []MaskCount {
	out := make([]MaskCount, 0, len(in))
	for _, c := range in {
		out = append(out, *c)
	}
	sortBy(out, func(a, b MaskCount) bool { return a.Places > b.Places || a.Places == b.Places && a.Name < b.Name })
	return out
}

func (s *Store) eachRow(ctx context.Context, query string, args []any, each func(*sql.Rows) error) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MaskRecordQuery picks records.
type MaskRecordQuery struct {
	Days int
	// Kind is a kind a record must have a value of; Facet a category
	// (secret, personal, word, rule) or what was done (warn, block).
	Kind, Facet     string
	Client, Channel string
	Model           string
	Before          string
	Limit           int
}

// MaskRecordPage is a page of records, newest first.
type MaskRecordPage struct {
	Items []MaskRecord `json:"items"`
	Next  string       `json:"next,omitempty"`
}

const recordColumns = `id, tenant_id, end_user_id, api_key_id, ts, client, channel, provider, model, places, distinct_values, restored, blocked, warned, took_us, values_json`

// MaskRecords lists scope's records a page at a time.
func (s *Store) MaskRecords(ctx context.Context, scope MaskScope, q MaskRecordQuery, now time.Time) (MaskRecordPage, error) {
	if s == nil || s.db == nil {
		return MaskRecordPage{}, ErrUnavailable
	}
	args := []any{}
	where := scope.where(&args, "")
	add := func(clause string, value any) {
		args = append(args, value)
		where += " AND " + strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args)))
	}
	start, _ := time.ParseInLocation("2006-01-02", firstMaskDay(now, q.Days), time.Local)
	add("id>=?", maskKeyFloor(start))
	if q.Before != "" {
		add("id<?", q.Before)
	}
	if q.Kind != "" {
		add(`kinds LIKE ? ESCAPE '\'`, "%,"+likeEscape(q.Kind)+",%")
	}
	if q.Facet != "" {
		add(`classes LIKE ? ESCAPE '\'`, "%,"+likeEscape(q.Facet)+",%")
	}
	if q.Client != "" {
		add("client=?", q.Client)
	}
	if q.Channel != "" {
		add("channel=?", q.Channel)
	}
	if q.Model != "" {
		add("model=?", q.Model)
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	page := MaskRecordPage{Items: []MaskRecord{}}
	err := s.eachRow(ctx, `SELECT `+recordColumns+` FROM privacy_mask_records WHERE `+where+` ORDER BY id DESC LIMIT `+strconv.Itoa(limit+1), args, func(rows *sql.Rows) error {
		record, err := scanMaskRecord(rows, false)
		if err != nil {
			return err
		}
		page.Items = append(page.Items, record)
		return nil
	})
	if err != nil {
		return MaskRecordPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[limit-1].Key
	}
	return page, nil
}

// ErrMaskRecordNotFound is returned for a record scope has none of.
var ErrMaskRecordNotFound = errors.New("privacy masking record not found")

// MaskRecord returns one of scope's records with its snippets.
func (s *Store) MaskRecord(ctx context.Context, scope MaskScope, key string) (MaskRecord, error) {
	if s == nil || s.db == nil {
		return MaskRecord{}, ErrUnavailable
	}
	args := []any{}
	where := scope.where(&args, "")
	args = append(args, key)
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordColumns+`, snippets_json FROM privacy_mask_records WHERE `+where+` AND id=$`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return MaskRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return MaskRecord{}, err
		}
		return MaskRecord{}, ErrMaskRecordNotFound
	}
	return scanMaskRecord(rows, true)
}

func scanMaskRecord(rows *sql.Rows, detail bool) (MaskRecord, error) {
	var r MaskRecord
	var ts, valuesJSON, snippetsJSON string
	var blocked int
	dest := []any{&r.Key, &r.TenantID, &r.EndUserID, &r.APIKeyID, &ts, &r.Client, &r.Channel, &r.Provider, &r.Model,
		&r.Places, &r.Distinct, &r.Restored, &blocked, &r.Warned, &r.TookMicros, &valuesJSON}
	if detail {
		dest = append(dest, &snippetsJSON)
	}
	if err := rows.Scan(dest...); err != nil {
		return MaskRecord{}, err
	}
	r.At, _ = time.Parse(time.RFC3339Nano, ts)
	r.Blocked = blocked != 0
	if valuesJSON != "" {
		if err := json.Unmarshal([]byte(valuesJSON), &r.Values); err != nil {
			return MaskRecord{}, err
		}
	}
	if r.Values == nil {
		r.Values = []MaskRecordValue{}
	}
	for i := range r.Values {
		r.Values[i].ID = ""
		if !detail {
			r.Values[i].Placeholder = "" // the list does not need them
		}
	}
	if detail && snippetsJSON != "" {
		_ = json.Unmarshal([]byte(snippetsJSON), &r.Snippets) // a snippet that does not read is not shown
	}
	return r, nil
}

// DeleteMaskRecords removes every record of scope, and its counts.
func (s *Store) DeleteMaskRecords(ctx context.Context, scope MaskScope) (int64, error) {
	if s == nil || s.db == nil {
		return 0, ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var deleted int64
	for i, table := range []string{"privacy_mask_records", "privacy_mask_value_days", "privacy_mask_days"} {
		args := []any{}
		result, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+scope.where(&args, ""), args...)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			deleted, _ = result.RowsAffected()
		}
	}
	return deleted, tx.Commit()
}

// MaskRecordRetention is how long records and counts are kept.
const MaskRecordRetention = 30 * 24 * time.Hour

// CleanupMaskRecords removes the records and counts older than the retention,
// and clears the snippets of the records older than contentDays days, the
// request log's retention of bodies (0: kept as long as the records), or of
// every record while bodies are not stored (keepSnippets false). It runs with
// the request log's own cleanup.
func CleanupMaskRecords(ctx context.Context, db *sql.DB, contentDays int, keepSnippets bool, now time.Time) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := now.Add(-MaskRecordRetention)
	result, err := db.ExecContext(ctx, `DELETE FROM privacy_mask_records WHERE id<$1`, maskKeyFloor(cutoff))
	if err != nil {
		return 0, err
	}
	deleted, _ := result.RowsAffected()
	day := maskDay(cutoff)
	for _, table := range []string{"privacy_mask_value_days", "privacy_mask_days"} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE day<$1`, day); err != nil {
			return deleted, err
		}
	}
	switch {
	case !keepSnippets:
		if _, err := db.ExecContext(ctx, `UPDATE privacy_mask_records SET snippets_json='' WHERE snippets_json<>''`); err != nil {
			return deleted, err
		}
	case contentDays > 0:
		if _, err := db.ExecContext(ctx, `UPDATE privacy_mask_records SET snippets_json='' WHERE id<$1 AND snippets_json<>''`,
			maskKeyFloor(now.AddDate(0, 0, -contentDays))); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// likeEscape keeps a value meant literally from acting as a LIKE pattern
// (with ESCAPE '\').
func likeEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(v)
}

func sortBy[T any](s []T, less func(a, b T) bool) {
	sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
}
