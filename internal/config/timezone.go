package config

import (
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// ResolveTimeLocation resolves an IANA timezone name (e.g. "Asia/Shanghai") into a time.Location.
// When timezone is empty, it returns time.Local.
// When timezone cannot be loaded, it logs a warning and returns time.Local.
func ResolveTimeLocation(timezone string) *time.Location {
	tz := strings.TrimSpace(timezone)
	if tz == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		log.Warnf("config: failed to load timezone %q: %v; using local timezone", tz, err)
		return time.Local
	}
	return loc
}

// ApplyTimeZone sets the process-local timezone for both Go (time.Local) and libraries that
// consult the TZ environment variable (e.g. SQLite localtime modifiers).
//
// If timezone is empty, this is a no-op and the current local timezone remains unchanged.
// It returns the resolved location (or time.Local when empty/invalid).
func ApplyTimeZone(timezone string) *time.Location {
	tz := strings.TrimSpace(timezone)
	loc := ResolveTimeLocation(tz)

	if tz != "" {
		_ = os.Setenv("TZ", tz)
		time.Local = loc
	}

	return loc
}
