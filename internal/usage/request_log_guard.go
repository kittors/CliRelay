package usage

import (
	"database/sql"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	log "github.com/sirupsen/logrus"
)

// request_logs.guard_kind records which loop protection check ended a request
// ("reply_loop": the reply was cut; "tool_loop": the gateway answered and sent
// nothing upstream). It is empty for every other request, and a request it is
// set on is also failed. Rows written before the column existed keep the empty
// default, which reads as "loop protection did not end this request".
//
// The set of values is closed: the monitor, the filters and the panel key on
// them, so a value this build does not know is stored as empty rather than as a
// kind nothing can read.

// migrateRequestLogGuardKindColumn adds the column to a SQLite database. The
// PostgreSQL schema gets it from the runtime migrations.
func migrateRequestLogGuardKindColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN guard_kind TEXT NOT NULL DEFAULT ''")
	if err != nil && !strings.Contains(err.Error(), "duplicate") {
		log.Warnf("usage: migrate column guard_kind: %v", err)
	}
}

// normalizeGuardKind returns kind if it is one this build knows, else "".
func normalizeGuardKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if loopdetect.Kind(kind).Valid() {
		return kind
	}
	return ""
}

// guardKindCondition renders the loop protection filter of a log query as one
// condition, empty when the query selects every request. Values this build does
// not know select nothing, never everything: a filter that silently widens
// would show the wrong rows under a label that says otherwise.
func guardKindCondition(kinds []string) (string, []any) {
	var known []any
	seen := make(map[string]struct{}, len(kinds))
	requested := false
	for _, kind := range kinds {
		if kind = strings.TrimSpace(kind); kind == "" {
			continue
		}
		requested = true
		if normalizeGuardKind(kind) == "" {
			continue
		}
		if _, dup := seen[kind]; dup {
			continue
		}
		seen[kind] = struct{}{}
		known = append(known, kind)
	}
	switch {
	case len(known) > 0:
		return "guard_kind IN (" + placeholders(len(known)) + ")", known
	case requested:
		return "1 = 0", nil
	default:
		return "", nil
	}
}
