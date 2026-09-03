package warmup

import (
	"time"
)

type PolicyStatus string

const (
	PolicyStatusPending    PolicyStatus = "pending"     // Waiting for StartAt
	PolicyStatusActive     PolicyStatus = "active"      // Currently running / eligible to trigger
	PolicyStatusQuietHours PolicyStatus = "quiet_hours" // In quiet period (e.g. night time)
	PolicyStatusCompleted  PolicyStatus = "completed"   // Reached StopAt
	PolicyStatusDisabled   PolicyStatus = "disabled"    // Manually turned off
)

// DailyTimeWindow defines active hours in a 24-hour clock (e.g. 07:00 to 23:00).
type DailyTimeWindow struct {
	Enabled     bool `json:"enabled"`
	StartHour   int  `json:"start_hour"`   // 0-23
	StartMinute int  `json:"start_minute"` // 0-59
	EndHour     int  `json:"end_hour"`     // 0-23
	EndMinute   int  `json:"end_minute"`   // 0-59
}

// IsWithin checks whether the given time falls within the active daily window.
func (w DailyTimeWindow) IsWithin(t time.Time) bool {
	if !w.Enabled {
		return true // No restriction
	}
	currentMinutes := t.Hour()*60 + t.Minute()
	startMinutes := w.StartHour*60 + w.StartMinute
	endMinutes := w.EndHour*60 + w.EndMinute

	if startMinutes <= endMinutes {
		// e.g. 07:00 to 23:00
		return currentMinutes >= startMinutes && currentMinutes < endMinutes
	}
	// Overnight window e.g. 22:00 to 06:00
	return currentMinutes >= startMinutes || currentMinutes < endMinutes
}

// Policy specifies an automated, staggered quota warmup schedule.
type Policy struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id"`
	Name            string          `json:"name"`
	Enabled         bool            `json:"enabled"`
	Providers       []string        `json:"providers"` // e.g. ["antigravity", "codex"]
	PoolIDs         []string        `json:"pool_ids"`  // e.g. ["antigravity:gemini", "antigravity:3p"]
	StartAt         *time.Time      `json:"start_at,omitempty"`
	StopAt          *time.Time      `json:"stop_at,omitempty"`
	DailyWindow     DailyTimeWindow `json:"daily_window"`
	IntervalSeconds int64           `json:"interval_seconds"` // e.g. 18000 (5h)
	StaggerMinutes  int             `json:"stagger_minutes"`  // Spread execution across N minutes
	Status          PolicyStatus    `json:"status"`
	LastRunAt       *time.Time      `json:"last_run_at,omitempty"`
	NextRunAt       *time.Time      `json:"next_run_at,omitempty"`
	TotalRuns       int64           `json:"total_runs"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}
