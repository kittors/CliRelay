package usage

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// PeriodResetAt reports when the calendar window of period that contains now
// ends, so a refusal can tell the client when to come back.
//
// The boundaries are the ones the usage queries count by: local midnight in the
// project timezone (the "today" of the daily limits), Monday 00:00 for the week
// and the 1st at 00:00 for the month (see PeriodWindowKeysAt). The project
// timezone is process-wide; no tenant has a timezone of its own.
//
// ok is false for periods that have no calendar end. The 5h window starts at a
// subject's first spend and is closed by the anchor, not the calendar, and the
// lifetime period never resets on its own.
func PeriodResetAt(period quota.Period, now time.Time) (time.Time, bool) {
	return periodResetAtLocation(period, now, getUsageLocation())
}

func periodResetAtLocation(period quota.Period, now time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	var end time.Time
	switch period {
	case quota.PeriodDay:
		end = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	case quota.PeriodWeek:
		// Weeks run Monday to Sunday, as PeriodWindowKeysAt counts them.
		sinceMonday := (int(local.Weekday()) + 6) % 7
		end = time.Date(local.Year(), local.Month(), local.Day()-sinceMonday+7, 0, 0, 0, 0, loc)
	case quota.PeriodMonth:
		end = time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	default:
		return time.Time{}, false
	}
	// A zone whose clocks skip midnight can leave the computed edge at or before
	// now; an answer that is already past is no answer.
	if !end.After(now) {
		return time.Time{}, false
	}
	return end, true
}
