package contentmoderation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	log "github.com/sirupsen/logrus"
)

// The records of privacy masking: one row per request masking found a value
// in, for the panel's list and detail, and two tables counted by day, for its
// summary. They describe values and never hold one: a value is its kind, what
// found it, its first characters, its length and an id (Value.ID, a keyed
// hash, the same across requests). The snippets of a record are the text
// around a value as the upstream got it, with placeholders in it; they are
// kept only while the request log keeps bodies, and cleared after its content
// retention.
//
// Records are written after the request is over, by a writer of their own
// (MaskRecords), so the reply has been put back and counted. It drops records
// rather than hold a request when the database is slow or down.

const privacyMaskRecordTablesSQL = `
CREATE TABLE IF NOT EXISTS privacy_mask_records (
  id TEXT NOT NULL PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  end_user_id TEXT NOT NULL DEFAULT '',
  api_key_id TEXT NOT NULL DEFAULT '',
  ts TEXT NOT NULL,
  client TEXT NOT NULL DEFAULT '',
  channel TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  places INTEGER NOT NULL DEFAULT 0,
  distinct_values INTEGER NOT NULL DEFAULT 0,
  restored INTEGER NOT NULL DEFAULT 0,
  blocked INTEGER NOT NULL DEFAULT 0,
  warned INTEGER NOT NULL DEFAULT 0,
  took_us INTEGER NOT NULL DEFAULT 0,
  kinds TEXT NOT NULL DEFAULT '',
  classes TEXT NOT NULL DEFAULT '',
  values_json TEXT NOT NULL DEFAULT '',
  snippets_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_privacy_mask_records_tenant ON privacy_mask_records (tenant_id, id);
CREATE INDEX IF NOT EXISTS idx_privacy_mask_records_end_user ON privacy_mask_records (tenant_id, end_user_id, id);
CREATE INDEX IF NOT EXISTS idx_privacy_mask_records_ts ON privacy_mask_records (ts);
CREATE TABLE IF NOT EXISTS privacy_mask_value_days (
  tenant_id TEXT NOT NULL,
  end_user_id TEXT NOT NULL DEFAULT '',
  api_key_id TEXT NOT NULL DEFAULT '',
  day TEXT NOT NULL,
  value_id TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT '',
  finder TEXT NOT NULL DEFAULT '',
  head TEXT NOT NULL DEFAULT '',
  value_len INTEGER NOT NULL DEFAULT 0,
  places INTEGER NOT NULL DEFAULT 0,
  requests INTEGER NOT NULL DEFAULT 0,
  last_ts TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, end_user_id, api_key_id, day, value_id)
);
CREATE INDEX IF NOT EXISTS idx_privacy_mask_value_days_day ON privacy_mask_value_days (day);
CREATE TABLE IF NOT EXISTS privacy_mask_days (
  tenant_id TEXT NOT NULL,
  end_user_id TEXT NOT NULL DEFAULT '',
  api_key_id TEXT NOT NULL DEFAULT '',
  day TEXT NOT NULL,
  client TEXT NOT NULL DEFAULT '',
  channel TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  requests INTEGER NOT NULL DEFAULT 0,
  places INTEGER NOT NULL DEFAULT 0,
  blocked INTEGER NOT NULL DEFAULT 0,
  warned INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, end_user_id, api_key_id, day, client, channel, model)
);
CREATE INDEX IF NOT EXISTS idx_privacy_mask_days_day ON privacy_mask_days (day);
`

// MaskRecordValue is one value of a record.
type MaskRecordValue struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind"`
	By     string `json:"by"`
	Action string `json:"action,omitempty"`
	Head   string `json:"head,omitempty"`
	Len    int    `json:"len"`
	N      int    `json:"n"`
	// Restored is how many places of the reply had it put back.
	Restored int `json:"restored,omitempty"`
	// Field is where in the request it was first found.
	Field string `json:"field,omitempty"`
	// Placeholder is what the upstream got in its place; only a record's
	// detail shows it.
	Placeholder string `json:"placeholder,omitempty"`
}

// MaskSnippet is text around values as the upstream got it.
type MaskSnippet struct {
	Field string `json:"field"`
	Role  string `json:"role,omitempty"`
	Text  string `json:"text"`
}

// MaskRecord is one request masking found a value in.
type MaskRecord struct {
	Key        string            `json:"key"`
	At         time.Time         `json:"at"`
	TenantID   string            `json:"-"`
	Client     string            `json:"client"`
	APIKeyID   string            `json:"api_key_id,omitempty"`
	APIKeyName string            `json:"api_key_name,omitempty"`
	EndUserID  string            `json:"end_user_id,omitempty"`
	EndUser    string            `json:"end_user_name,omitempty"`
	Channel    string            `json:"channel,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Model      string            `json:"model,omitempty"`
	Places     int               `json:"places"`
	Distinct   int               `json:"distinct"`
	Restored   int               `json:"restored,omitempty"`
	Blocked    bool              `json:"blocked,omitempty"`
	Warned     int               `json:"warned,omitempty"`
	TookMicros int64             `json:"took_us,omitempty"`
	Values     []MaskRecordValue `json:"values"`
	Snippets   []MaskSnippet     `json:"snippets,omitempty"`
}

// newMaskKey is a record's key: the time it was made, then random bits, in
// hex, so keys sort as the records were made and the list pages by key.
func newMaskKey(at time.Time) string {
	var tail [4]byte
	_, _ = rand.Read(tail[:])
	return fmt.Sprintf("%016x%08x", at.UnixNano(), binary.BigEndian.Uint32(tail[:]))
}

// maskKeyFloor is the smallest key of a record made at or after t.
func maskKeyFloor(t time.Time) string { return fmt.Sprintf("%016x%08x", t.UnixNano(), 0) }

// valueClass is the facet a value is listed under: its kind's category, or
// "rule" for one the tenant's or the user's own rules found.
func valueClass(v MaskRecordValue) string {
	if v.By == privacymask.ByYours {
		return "rule"
	}
	return privacymask.Class(v.Kind)
}

// maskDay is the day a record counts on, in the server's time zone, as the
// usage pages count theirs.
func maskDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// ---- The writer ----

const (
	maskRecordQueue = 2048
	maskRecordBatch = 128
	maskRecordFlush = time.Second
)

// MaskRecordWriter writes records in batches, in the background.
type MaskRecordWriter struct {
	once    sync.Once
	store   atomic.Pointer[Store]
	queue   chan *MaskRecord
	dropped atomic.Int64
	logs    logLimiter
}

var defaultMaskRecords = &MaskRecordWriter{queue: make(chan *MaskRecord, maskRecordQueue)}

// MaskRecords returns the writer of this process.
func MaskRecords() *MaskRecordWriter { return defaultMaskRecords }

// SetStore attaches the store the records go to, and starts the writer.
func (w *MaskRecordWriter) SetStore(store *Store) {
	w.store.Store(store)
	w.once.Do(func() { go w.run() })
}

// Add queues a record; a full queue drops it.
func (w *MaskRecordWriter) Add(record *MaskRecord) {
	if record == nil || w.store.Load() == nil {
		return
	}
	select {
	case w.queue <- record:
	default:
		w.drop(1, "the queue is full")
	}
}

func (w *MaskRecordWriter) drop(n int, why string) {
	total := w.dropped.Add(int64(n))
	if ok, _ := w.logs.allow("privacy_mask_records", wordHitLogInterval, time.Now()); ok {
		log.WithField("dropped_total", total).Warnf("privacy masking: %d record(s) were not written: %s", n, why)
	}
}

func (w *MaskRecordWriter) run() {
	timer := time.NewTimer(maskRecordFlush)
	defer timer.Stop()
	var batch []*MaskRecord
	for {
		select {
		case record := <-w.queue:
			batch = append(batch, record)
			if len(batch) < maskRecordBatch {
				continue
			}
		case <-timer.C:
			timer.Reset(maskRecordFlush)
			if len(batch) == 0 {
				continue
			}
		}
		if err := w.write(batch); err != nil {
			w.drop(len(batch), err.Error())
		}
		batch = batch[:0]
	}
}

// Flush writes what is queued now, for a writer whose loop is not running
// (tests): with the loop running, it may take the records first.
func (w *MaskRecordWriter) Flush() error {
	var batch []*MaskRecord
	for {
		select {
		case record := <-w.queue:
			batch = append(batch, record)
		default:
			if len(batch) == 0 {
				return nil
			}
			return w.write(batch)
		}
	}
}

func (w *MaskRecordWriter) write(batch []*MaskRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.store.Load().WriteMaskRecords(ctx, batch)
}

// WriteMaskRecords writes records and adds them to the counts, in one
// transaction.
func (s *Store) WriteMaskRecords(ctx context.Context, batch []*MaskRecord) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, record := range batch {
		if err := insertMaskRecord(ctx, tx, record); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertMaskRecord(ctx context.Context, tx *sql.Tx, r *MaskRecord) error {
	kinds, classes := map[string]bool{}, map[string]bool{}
	for _, v := range r.Values {
		kinds[v.Kind] = true
		classes[valueClass(v)] = true
		if v.Action != "" {
			classes[v.Action] = true
		}
	}
	if r.Blocked {
		classes[privacymask.Block] = true
	}
	values, err := json.Marshal(r.Values)
	if err != nil {
		return err
	}
	snippets := ""
	if len(r.Snippets) > 0 {
		raw, err := json.Marshal(r.Snippets)
		if err != nil {
			return err
		}
		snippets = string(raw)
	}
	ts := r.At.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO privacy_mask_records (id,tenant_id,end_user_id,api_key_id,ts,client,channel,provider,model,places,distinct_values,restored,blocked,warned,took_us,kinds,classes,values_json,snippets_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT(id) DO NOTHING`,
		r.Key, r.TenantID, r.EndUserID, r.APIKeyID, ts, r.Client, r.Channel, r.Provider, r.Model,
		r.Places, r.Distinct, r.Restored, boolColumn(r.Blocked), r.Warned, r.TookMicros,
		joinedSet(kinds), joinedSet(classes), string(values), snippets); err != nil {
		return err
	}
	day := maskDay(r.At)
	for _, v := range r.Values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO privacy_mask_value_days (tenant_id,end_user_id,api_key_id,day,value_id,kind,finder,head,value_len,places,requests,last_ts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,1,$11)
			ON CONFLICT(tenant_id,end_user_id,api_key_id,day,value_id) DO UPDATE SET
			  places=privacy_mask_value_days.places+excluded.places,
			  requests=privacy_mask_value_days.requests+1,
			  kind=excluded.kind, finder=excluded.finder, head=excluded.head, value_len=excluded.value_len,
			  last_ts=CASE WHEN excluded.last_ts > privacy_mask_value_days.last_ts THEN excluded.last_ts ELSE privacy_mask_value_days.last_ts END`,
			r.TenantID, r.EndUserID, r.APIKeyID, day, v.ID, v.Kind, v.By, v.Head, v.Len, v.N, ts); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO privacy_mask_days (tenant_id,end_user_id,api_key_id,day,client,channel,model,provider,requests,places,blocked,warned)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10,$11)
		ON CONFLICT(tenant_id,end_user_id,api_key_id,day,client,channel,model) DO UPDATE SET
		  requests=privacy_mask_days.requests+1,
		  places=privacy_mask_days.places+excluded.places,
		  blocked=privacy_mask_days.blocked+excluded.blocked,
		  warned=privacy_mask_days.warned+excluded.warned,
		  provider=excluded.provider`,
		r.TenantID, r.EndUserID, r.APIKeyID, day, r.Client, r.Channel, r.Model, r.Provider, r.Places, boolColumn(r.Blocked), r.Warned)
	return err
}

// joinedSet writes a set as ",a,b,", so a LIKE '%,a,%' finds a row with a.
func joinedSet(set map[string]bool) string {
	if len(set) == 0 {
		return ""
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return "," + strings.Join(keys, ",") + ","
}

// DeleteMaskUserRecords removes the records of a portal user who has been
// deleted.
func (s *Store) DeleteMaskUserRecords(ctx context.Context, tenantID, endUserID string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tenantID, endUserID = strings.TrimSpace(tenantID), strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	for _, table := range []string{"privacy_mask_records", "privacy_mask_value_days", "privacy_mask_days"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id=$1 AND end_user_id=$2`, tenantID, endUserID); err != nil {
			return err
		}
	}
	return nil
}
