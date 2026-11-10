package authfiles

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

var (
	lastRefreshKeys               = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}
	subscriptionStartKeys         = []string{"subscription_started_at", "subscriptionStartedAt", "subscription_start_at", "subscriptionStartAt"}
	subscriptionPeriodKeys        = []string{"subscription_period", "subscriptionPeriod"}
	subscriptionExpirationKeys    = []string{"subscription_expires_at", "subscriptionExpiresAt"}
	subscriptionExpirationLayouts = []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04",
	}
)

// ExtractLastRefreshTimestamp returns the normalized last refresh timestamp from
// auth metadata while accepting legacy key spellings used by older auth files.
func ExtractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := ParseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// ParseLastRefreshValue normalizes string, number, and json.Number timestamps.
func ParseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

// ExtractSubscriptionExpirationTimestamp returns an explicit subscription
// expiration timestamp from metadata, accepting legacy key spellings.
func ExtractSubscriptionExpirationTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range subscriptionExpirationKeys {
		if val, ok := meta[key]; ok {
			if ts, okParse := ParseSubscriptionTimestampValue(val); okParse && !ts.IsZero() {
				return ts.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// ExtractSubscriptionStartTimestamp returns the subscription start timestamp
// from metadata, accepting legacy key spellings.
func ExtractSubscriptionStartTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range subscriptionStartKeys {
		if val, ok := meta[key]; ok {
			if ts, okParse := ParseSubscriptionTimestampValue(val); okParse && !ts.IsZero() {
				return ts.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// ExtractSubscriptionPeriod returns "monthly" or "yearly" from metadata.
func ExtractSubscriptionPeriod(meta map[string]any) (string, bool) {
	if len(meta) == 0 {
		return "", false
	}
	for _, key := range subscriptionPeriodKeys {
		if val, ok := meta[key]; ok {
			if period, okParse := NormalizeSubscriptionPeriodValue(val); okParse {
				return period, true
			}
		}
	}
	return "", false
}

// NormalizeSubscriptionPeriodValue maps legacy period values onto the public
// monthly/yearly response values.
func NormalizeSubscriptionPeriodValue(v any) (string, bool) {
	switch val := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "monthly", "month":
			return "monthly", true
		case "yearly", "annual", "annually", "year":
			return "yearly", true
		default:
			return "", false
		}
	case json.Number:
		i, err := val.Int64()
		if err != nil {
			return "", false
		}
		if i == 12 {
			return "yearly", true
		}
		if i == 1 {
			return "monthly", true
		}
	case float64:
		if val == 12 {
			return "yearly", true
		}
		if val == 1 {
			return "monthly", true
		}
	case int:
		if val == 12 {
			return "yearly", true
		}
		if val == 1 {
			return "monthly", true
		}
	case int64:
		if val == 12 {
			return "yearly", true
		}
		if val == 1 {
			return "monthly", true
		}
	}
	return "", false
}

// ParseSubscriptionTimestampValue accepts RFC3339-like strings and Unix
// timestamps in seconds or milliseconds.
func ParseSubscriptionTimestampValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		for _, layout := range subscriptionExpirationLayouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
			return NormalizeSubscriptionUnix(unix), true
		}
	case float64:
		return NormalizeSubscriptionUnix(int64(val)), true
	case int64:
		return NormalizeSubscriptionUnix(val), true
	case int:
		return NormalizeSubscriptionUnix(int64(val)), true
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return NormalizeSubscriptionUnix(i), true
		}
		if f, err := val.Float64(); err == nil {
			return NormalizeSubscriptionUnix(int64(f)), true
		}
	}
	return time.Time{}, false
}

// NormalizeSubscriptionUnix converts positive second or millisecond Unix values
// to UTC timestamps.
func NormalizeSubscriptionUnix(raw int64) time.Time {
	if raw <= 0 {
		return time.Time{}
	}
	if raw > 1_000_000_000_000 {
		return time.UnixMilli(raw).UTC()
	}
	return time.Unix(raw, 0).UTC()
}

// MetadataString returns the first non-empty string value for the provided keys.
func MetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if raw, ok := metadata[key].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

// SubscriptionExpirationFromStart derives the expiration timestamp for monthly
// and yearly subscription periods.
func SubscriptionExpirationFromStart(startedAt time.Time, period string) time.Time {
	months := 1
	if strings.EqualFold(period, "yearly") {
		months = 12
	}
	// Add the period from day one so a short target month cannot overflow
	// into the following month (for example, January 31 into March).
	first := startedAt.AddDate(0, months, 1-startedAt.Day())
	lastDay := first.AddDate(0, 1, -1).Day()
	day := startedAt.Day()
	if day > lastDay {
		day = lastDay
	}
	return first.AddDate(0, 0, day-1).UTC()
}

// SubscriptionRemainingMinutes rounds away from zero to match the management API
// countdown fields.
func SubscriptionRemainingMinutes(now, expiresAt time.Time) int64 {
	diff := expiresAt.Sub(now)
	if diff == 0 {
		return 0
	}
	if diff > 0 {
		return int64((diff + time.Minute - time.Nanosecond) / time.Minute)
	}
	return -int64((-diff + time.Minute - time.Nanosecond) / time.Minute)
}

// SubscriptionRemainingDays rounds away from zero to match the management API
// countdown fields.
func SubscriptionRemainingDays(now, expiresAt time.Time) int64 {
	diff := expiresAt.Sub(now)
	if diff == 0 {
		return 0
	}
	day := 24 * time.Hour
	if diff > 0 {
		return int64((diff + day - time.Nanosecond) / day)
	}
	return -int64((-diff + day - time.Nanosecond) / day)
}

// AddSubscriptionFields adds the public subscription fields used by the
// management auth-file list/detail responses.
func AddSubscriptionFields(entry map[string]any, meta map[string]any, now time.Time) {
	startedAt, ok := ExtractSubscriptionStartTimestamp(meta)
	if !ok {
		addSubscriptionExpirationFields(entry, meta, now)
		return
	}
	period, ok := ExtractSubscriptionPeriod(meta)
	if !ok {
		period = "monthly"
	}
	expiresAt := SubscriptionExpirationFromStart(startedAt, period)
	entry["subscription_started_at"] = startedAt.Format(time.RFC3339)
	entry["subscription_started_at_ms"] = startedAt.UnixMilli()
	entry["subscription_period"] = period
	entry["subscription_expires_at"] = expiresAt.Format(time.RFC3339)
	entry["subscription_expires_at_ms"] = expiresAt.UnixMilli()
	entry["subscription_remaining_days"] = SubscriptionRemainingDays(now, expiresAt)
	entry["subscription_remaining_minutes"] = SubscriptionRemainingMinutes(now, expiresAt)
	entry["subscription_expired"] = !now.Before(expiresAt)
}

func addSubscriptionExpirationFields(entry map[string]any, meta map[string]any, now time.Time) {
	expiresAt, ok := ExtractSubscriptionExpirationTimestamp(meta)
	if !ok {
		return
	}
	entry["subscription_expires_at"] = expiresAt.Format(time.RFC3339)
	entry["subscription_expires_at_ms"] = expiresAt.UnixMilli()
	entry["subscription_remaining_days"] = SubscriptionRemainingDays(now, expiresAt)
	entry["subscription_remaining_minutes"] = SubscriptionRemainingMinutes(now, expiresAt)
	entry["subscription_expired"] = !now.Before(expiresAt)
}

// DeleteSubscriptionStartMetadata removes canonical and legacy subscription
// start keys from auth metadata.
func DeleteSubscriptionStartMetadata(meta map[string]any) {
	for _, key := range subscriptionStartKeys {
		delete(meta, key)
	}
	delete(meta, "subscription_started_at_ms")
	delete(meta, "subscriptionStartedAtMs")
}

// DeleteSubscriptionPeriodMetadata removes canonical and legacy subscription
// period keys from auth metadata.
func DeleteSubscriptionPeriodMetadata(meta map[string]any) {
	for _, key := range subscriptionPeriodKeys {
		delete(meta, key)
	}
}

// DeleteSubscriptionExpirationMetadata removes canonical, legacy, and derived
// subscription expiration keys from auth metadata.
func DeleteSubscriptionExpirationMetadata(meta map[string]any) {
	for _, key := range subscriptionExpirationKeys {
		delete(meta, key)
	}
	delete(meta, "subscription_expires_at_ms")
	delete(meta, "subscriptionExpiresAtMs")
	delete(meta, "subscription_remaining_minutes")
	delete(meta, "subscriptionRemainingMinutes")
	delete(meta, "subscription_remaining_days")
	delete(meta, "subscriptionRemainingDays")
	delete(meta, "subscription_expired")
	delete(meta, "subscriptionExpired")
}

// ClearSubscriptionMetadata removes all stored and derived subscription fields.
func ClearSubscriptionMetadata(meta map[string]any) {
	DeleteSubscriptionStartMetadata(meta)
	DeleteSubscriptionPeriodMetadata(meta)
	DeleteSubscriptionExpirationMetadata(meta)
}
