package usage

import (
	"database/sql"
	"strings"

	log "github.com/sirupsen/logrus"
)

func migrateUpstreamModelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN upstream_model TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate") {
		log.Warnf("usage: migrate column upstream_model: %v", err)
	}
}

// migrateUpstreamResponseModelColumn adds the column recording the model an
// upstream declared in its own response. Rows written before this column existed
// keep the empty default, which reads as "the upstream never declared one" and
// so is never reported as a mismatch.
func migrateUpstreamResponseModelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN upstream_response_model TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate") {
		log.Warnf("usage: migrate column upstream_response_model: %v", err)
	}
}

func migrateVisionFallbackModelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN vision_fallback_model TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate") {
		log.Warnf("usage: migrate column vision_fallback_model: %v", err)
	}
}

func migrateThinkingLevelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN thinking_level TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate") {
		log.Warnf("usage: migrate column thinking_level: %v", err)
	}
}
