package usage

import (
	"database/sql"
	"strings"

	log "github.com/sirupsen/logrus"
)

// SQLite upgrades of request_log_content for databases created before a column
// or index existed; createTableSQL has them for new ones. PostgreSQL gets the
// same through internal/storage/postgres migrations.

func migrateRequestLogDetailColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_log_content ADD COLUMN detail_content BLOB NOT NULL DEFAULT X''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column detail_content: %v", err)
		}
	}
}

func migrateRequestLogContentSessionIDColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_log_content ADD COLUMN session_id TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column session_id: %v", err)
		}
	}
}

// migrateRequestLogContentBodiesStrippedColumn adds the flag the body purge
// reads (see purgeStoredRequestBodies). Rows that predate it read 0, "may hold
// bodies", so the first purge after the upgrade checks each of them once.
func migrateRequestLogContentBodiesStrippedColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_log_content ADD COLUMN bodies_stripped INTEGER NOT NULL DEFAULT 0")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column bodies_stripped: %v", err)
		}
	}
}

// ensureRequestLogContentIndexes runs after the column migrations above, since
// the partial indexes name their columns.
func ensureRequestLogContentIndexes(db *sql.DB) {
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_log_content_detail_timestamp
		ON request_log_content(timestamp DESC)
		WHERE length(detail_content) > 0
	`); err != nil {
		log.Warnf("usage: create idx_log_content_detail_timestamp: %v", err)
	}
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_log_content_session_timestamp
		ON request_log_content(session_id, timestamp DESC)
		WHERE session_id <> ''
	`); err != nil {
		log.Warnf("usage: create idx_log_content_session_timestamp: %v", err)
	}
	// The rows the body purge has left to check; empty while body storage
	// stays off. See nextUnstrippedRequestLogIDs.
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_log_content_bodies_unstripped
		ON request_log_content(log_id)
		WHERE bodies_stripped = 0
	`); err != nil {
		log.Warnf("usage: create idx_log_content_bodies_unstripped: %v", err)
	}
}
