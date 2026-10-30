package authfiles

import (
	"testing"
	"time"
)

func TestAddSubscriptionFieldsClampsCalendarExpiration(t *testing.T) {
	tests := []struct{ name, start, period, want string }{
		{"january month end", "2026-01-31T09:30:00Z", "monthly", "2026-02-28T09:30:00Z"},
		{"leap February", "2024-01-31T09:30:00Z", "monthly", "2024-02-29T09:30:00Z"},
		{"thirty day month", "2026-03-31T09:30:00Z", "monthly", "2026-04-30T09:30:00Z"},
		{"yearly leap day", "2024-02-29T09:30:00Z", "yearly", "2025-02-28T09:30:00Z"},
		{"ordinary monthly", "2026-04-15T09:30:00Z", "monthly", "2026-05-15T09:30:00Z"},
		{"year rollover", "2026-12-31T09:30:00Z", "monthly", "2027-01-31T09:30:00Z"},
		{"ordinary yearly", "2026-04-15T09:30:00Z", "yearly", "2027-04-15T09:30:00Z"},
		{"UTC offset normalized", "2026-02-01T01:30:00+08:00", "monthly", "2026-02-28T17:30:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expires, err := time.Parse(time.RFC3339, tt.want)
			if err != nil {
				t.Fatal(err)
			}
			for _, delta := range []time.Duration{-time.Minute, 0, time.Minute} {
				now := expires.Add(delta)
				entry := map[string]any{}
				AddSubscriptionFields(entry, map[string]any{"subscription_started_at": tt.start, "subscription_period": tt.period}, now)
				if got := entry["subscription_expires_at"]; got != tt.want {
					t.Errorf("expiration = %v, want %s", got, tt.want)
				}
				if got := entry["subscription_expires_at_ms"]; got != expires.UnixMilli() {
					t.Errorf("expiration milliseconds = %v, want %d", got, expires.UnixMilli())
				}
				if got := entry["subscription_expired"]; got != (delta >= 0) {
					t.Errorf("expired at delta %s = %v, want %v", delta, got, delta >= 0)
				}
				if got := entry["subscription_remaining_minutes"]; got != int64(-delta/time.Minute) {
					t.Errorf("remaining minutes = %v, want %d", got, int64(-delta/time.Minute))
				}
			}
		})
	}
}
