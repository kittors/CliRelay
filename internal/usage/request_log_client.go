package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

// request_logs.client_name, client_version and user_agent name the caller of a
// request: the client internal/clientident recognised from the inbound headers
// ("codex-desktop", "claude-code", or the first product token of an unknown
// User-Agent such as "okhttp"), the version it reported and the raw User-Agent.
// The panel's Agent column and filter read them. Rows written before the
// columns existed, and requests that carried no User-Agent, keep the empty
// defaults and show as unknown.

// ClientNameUnknown is the facet value and filter value for rows without a
// client name. client_name never holds it: names keep only letters, digits,
// dots and hyphens.
const ClientNameUnknown = "__unknown__"

// clientNameFacetLimit bounds the facet. The User-Agent is caller-controlled,
// so one API key could otherwise make every request a new option.
const clientNameFacetLimit = 100

const (
	maxClientNameBytes    = 40
	maxClientVersionBytes = 48
	maxUserAgentBytes     = 256
)

// migrateRequestLogClientColumns adds the columns to a SQLite database. The
// PostgreSQL schema gets them from the runtime migrations.
func migrateRequestLogClientColumns(db *sql.DB) {
	for _, column := range []string{"client_name", "client_version", "user_agent"} {
		_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''")
		if err != nil && !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column %s: %v", column, err)
		}
	}
}

// normalizeRequestLogClient bounds the caller fields again before they are
// written. The executor already bounds what it identifies, but entries also
// arrive from the spool and from callers of InsertRequestLog.
func normalizeRequestLogClient(entry RequestLogEntry) RequestLogEntry {
	entry.ClientName = clipUTF8(strings.ToLower(strings.TrimSpace(entry.ClientName)), maxClientNameBytes)
	if entry.ClientName == ClientNameUnknown {
		entry.ClientName = ""
	}
	entry.ClientVersion = clipUTF8(strings.TrimSpace(entry.ClientVersion), maxClientVersionBytes)
	entry.UserAgent = clipUTF8(strings.TrimSpace(entry.UserAgent), maxUserAgentBytes)
	return entry
}

func clipUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// clientNameCondition renders the Agent filter of a log query as one
// condition, empty when the query selects every client. ClientNameUnknown
// selects the rows without a client name.
func clientNameCondition(names []string) (string, []any) {
	var known []any
	unknown := false
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		switch {
		case name == "":
			continue
		case name == ClientNameUnknown:
			unknown = true
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		known = append(known, name)
	}
	switch {
	case len(known) > 0 && unknown:
		return "(client_name IN (" + placeholders(len(known)) + ") OR client_name = '')", known
	case len(known) > 0:
		return "client_name IN (" + placeholders(len(known)) + ")", known
	case unknown:
		return "client_name = ''", nil
	default:
		return "", nil
	}
}

func appendClientNameCondition(conditions []string, args []interface{}, names []string) ([]string, []interface{}) {
	if condition, nameArgs := clientNameCondition(names); condition != "" {
		return append(conditions, condition), append(args, nameArgs...)
	}
	return conditions, args
}

// queryDistinctClientNames lists the clients of the filtered window with their
// request counts, most requests first, ClientNameUnknown standing for the rows
// without one.
func queryDistinctClientNames(db *sql.DB, params LogQueryParams) ([]string, map[string]int64, error) {
	where, args := buildWhereClause(params)
	q := "SELECT client_name, COUNT(*) AS requests FROM request_logs" + where +
		fmt.Sprintf(" GROUP BY client_name ORDER BY requests DESC, client_name LIMIT %d", clientNameFacetLimit)
	rows, err := db.Query(q, args...)
	if err != nil {
		log.Warnf("usage: distinct client_name query failed: %v", err)
		return nil, nil, fmt.Errorf("usage: distinct client_name: %w", err)
	}
	defer rows.Close()

	names := make([]string, 0)
	counts := make(map[string]int64)
	for rows.Next() {
		var name string
		var requests int64
		if err := rows.Scan(&name, &requests); err != nil {
			return nil, nil, fmt.Errorf("usage: distinct client_name scan: %w", err)
		}
		if name == "" {
			name = ClientNameUnknown
		}
		names = append(names, name)
		counts[name] = requests
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("usage: distinct client_name rows: %w", err)
	}
	return names, counts, nil
}
