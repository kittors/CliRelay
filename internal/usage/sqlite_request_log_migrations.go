package usage

import (
	"database/sql"

	log "github.com/sirupsen/logrus"
)

// runSQLiteRequestLogColumnMigrations brings an existing SQLite usage database
// up to the current request log schema, in the order the columns and indexes
// were introduced. Every step is idempotent, so it runs on each start. The
// PostgreSQL schema gets the same columns from the runtime migrations.
func runSQLiteRequestLogColumnMigrations(db *sql.DB) {
	log.Debugf("usage: running tenant scope column migration")
	migrateRequestLogTenantColumns(db)
	log.Debugf("usage: running quota tenant scope column migration")
	migrateQuotaTenantColumns(db)
	log.Debugf("usage: running content column migration")
	migrateContentColumns(db)
	log.Debugf("usage: running cost column migration")
	migrateCostColumn(db)
	log.Debugf("usage: running api_key_name column migration")
	migrateApiKeyNameColumn(db)
	log.Debugf("usage: running upstream_model column migration")
	migrateUpstreamModelColumn(db)
	log.Debugf("usage: running upstream_response_model column migration")
	migrateUpstreamResponseModelColumn(db)
	log.Debugf("usage: running vision_fallback_model column migration")
	migrateVisionFallbackModelColumn(db)
	log.Debugf("usage: running thinking_level column migration")
	migrateThinkingLevelColumn(db)
	log.Debugf("usage: running api_key_id column migration")
	migrateAPIKeyIDColumn(db)
	log.Debugf("usage: ensuring request log lookup indexes")
	ensureRequestLogLookupIndexes(db)
	log.Debugf("usage: running auth_subject_id column migration")
	migrateAuthSubjectIDColumns(db)
	log.Debugf("usage: rebuilding quota tenant primary keys")
	migrateQuotaTenantPrimaryKeys(db)
	log.Debugf("usage: running first_token_ms column migration")
	migrateFirstTokenColumn(db)
	log.Debugf("usage: running streaming column migration")
	migrateStreamingColumn(db)
	log.Debugf("usage: running request log detail column migration")
	migrateRequestLogDetailColumn(db)
	log.Debugf("usage: running request log content session_id column migration")
	migrateRequestLogContentSessionIDColumn(db)
	log.Debugf("usage: running request log content bodies_stripped column migration")
	migrateRequestLogContentBodiesStrippedColumn(db)
	log.Debugf("usage: running request log cache and session column migration")
	migrateRequestLogCacheColumns(db)
	log.Debugf("usage: running request log guard_kind column migration")
	migrateRequestLogGuardKindColumn(db)
	log.Debugf("usage: running request log client columns migration")
	migrateRequestLogClientColumns(db)
	log.Debugf("usage: ensuring request log content indexes")
	ensureRequestLogContentIndexes(db)
}
