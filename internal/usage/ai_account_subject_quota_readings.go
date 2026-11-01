package usage

import (
	"database/sql"
	"fmt"
	"time"
)

// AIAccountQuotaReading is one stored reading of one quota window of a physical
// account, as a status probe left it.
type AIAccountQuotaReading struct {
	AuthSubjectID string
	QuotaKey      string
	// RemainingPercent is the share left as the probe reported it; nil when the
	// upstream gave no percentage.
	RemainingPercent *float64
	// ResetAt is when the reading said the window renews; zero when it did not say.
	ResetAt    time.Time
	RecordedAt time.Time
}

// quotaReadingsChunk bounds how many accounts one query names. Together with
// the window keys it stays far below the bind-variable limit of either engine.
const quotaReadingsChunk = 200

// QueryAIAccountSubjectQuotaReadings loads the readings of the named windows of
// the named accounts recorded at or after since, oldest first.
//
// It is one indexed query per chunk of accounts (the table is indexed by
// account, window and time), and reads from the read pool: the pool-capacity view
// calls it with every account of a tenant at once, where asking per account
// would be hundreds of round trips.
func QueryAIAccountSubjectQuotaReadings(subjectIDs, quotaKeys []string, since time.Time) ([]AIAccountQuotaReading, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	keys := dedupeExactStrings(quotaKeys)
	if db == nil || len(ids) == 0 || len(keys) == 0 {
		return nil, nil
	}
	var out []AIAccountQuotaReading
	for start := 0; start < len(ids); start += quotaReadingsChunk {
		chunk := ids[start:min(start+quotaReadingsChunk, len(ids))]
		readings, err := queryQuotaReadingsChunk(db, chunk, keys, since)
		if err != nil {
			return nil, err
		}
		out = append(out, readings...)
	}
	return out, nil
}

func queryQuotaReadingsChunk(db *sql.DB, ids, keys []string, since time.Time) ([]AIAccountQuotaReading, error) {
	args := make([]any, 0, len(ids)+len(keys)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, key := range keys {
		args = append(args, key)
	}
	args = append(args, since.UTC().Format(time.RFC3339Nano))
	rows, err := db.Query(`
		SELECT auth_subject_id, quota_key, percent, reset_at, recorded_at
		FROM ai_account_subject_quota_points
		WHERE auth_subject_id IN (`+placeholders(len(ids))+`)
		  AND quota_key IN (`+placeholders(len(keys))+`)
		  AND recorded_at >= ?
		ORDER BY recorded_at ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: shared quota readings: %w", err)
	}
	defer rows.Close()

	var out []AIAccountQuotaReading
	for rows.Next() {
		var reading AIAccountQuotaReading
		var percent sql.NullFloat64
		var reset, recorded storedTime
		if err := rows.Scan(&reading.AuthSubjectID, &reading.QuotaKey, &percent, &reset, &recorded); err != nil {
			return nil, err
		}
		if !recorded.Valid {
			continue
		}
		reading.RecordedAt = recorded.Time
		if percent.Valid {
			value := percent.Float64
			reading.RemainingPercent = &value
		}
		if reset.Valid {
			reading.ResetAt = reset.Time
		}
		out = append(out, reading)
	}
	return out, rows.Err()
}
