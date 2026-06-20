package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// providerEchoModelPattern is the SQLite LIKE pattern that marks request_logs.model
// values that are upstream provider-internal paths echoed back in an
// OpenAI-compatible response's "model" field (e.g. "accounts/fireworks/models/glm-5p2").
// These are not valid model names for display, pricing lookup or filtering, and were
// persisted because the OpenAI-compat executor used to override the reporter's clean
// request-time model with this echo. The override has been removed; this backfill
// repairs the rows already written with the bad value.
//
// Only the "accounts/<x>/models/<y>" shape is treated as bad. Other slash model names
// (e.g. "meta/llama-3.1-8b-instruct") are legitimate upstream conventions and are
// left untouched.
const providerEchoModelPattern = "accounts/%/models/%"

// backfillRequestLogModelNames repairs request_logs rows whose model column holds an
// upstream provider-echo path. It recovers the original client-requested model from
// the stored request body (request_log_content.input_content, or the legacy
// request_logs.input_content fallback) and rewrites both the model and the cost (which
// was zero because the echo string has no pricing entry).
// This is intentionally not run from InitDB: historical content rows can be large or
// damaged, and service startup must not depend on decompressing old request bodies.
//
// Rows whose stored request body has been cleared cannot be recovered: they are left
// as-is and counted in a warning so the operator can decide what to do. The function is
// idempotent: only rows still matching the bad pattern are considered, so re-running
// after a successful backfill is a no-op (recovered rows no longer match).
func backfillRequestLogModelNames(db *sql.DB) {
	if db == nil {
		return
	}

	const batchSize = 200
	backfilled := 0
	unrecoverable := 0
	// Cursor: only look at rows with id greater than the last id inspected. This
	// guarantees forward progress even when some rows cannot be recovered (they keep
	// matching the bad pattern and would otherwise reappear every batch).
	var lastID int64

	for {
		processed, unrecoverableThisBatch, lastSeen, err := backfillRequestLogModelNamesBatch(db, batchSize, lastID)
		if err != nil {
			log.Warnf("usage: backfill request_logs model batch failed: %v", err)
			return
		}
		backfilled += processed
		unrecoverable += unrecoverableThisBatch
		if lastSeen > lastID {
			lastID = lastSeen
		}
		if processed == 0 && unrecoverableThisBatch == 0 {
			break
		}
	}

	if backfilled > 0 {
		log.Infof("usage: backfilled model for %d request_logs rows from stored request body", backfilled)
	}
	if unrecoverable > 0 {
		log.Warnf("usage: %d request_logs rows still hold a provider-echo model but have no stored request body to recover the clean model from; left unchanged", unrecoverable)
	}
}

// backfillRequestLogModelNamesBatch processes up to batchSize rows whose id is greater
// than afterID. It returns the number of rows rewritten, the number of rows that
// matched the bad pattern but could not be recovered, the highest id inspected, and
// any error.
func backfillRequestLogModelNamesBatch(db *sql.DB, batchSize int, afterID int64) (rewritten, unrecoverable int, lastID int64, err error) {
	if db == nil {
		return 0, 0, 0, nil
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	// Prefer the compressed content row; fall back to the legacy uncompressed
	// input_content column on request_logs for older rows.
	rows, err := db.Query(
		`SELECT logs.id,
		        logs.input_tokens, logs.output_tokens, logs.reasoning_tokens,
		        logs.cached_tokens, logs.total_tokens,
		        coalesce(content.compression, '')   AS compression,
		        coalesce(content.input_content, '') AS content_input,
		        logs.input_content                  AS legacy_input
		 FROM request_logs logs
		 LEFT JOIN request_log_content content ON content.log_id = logs.id
		 WHERE logs.model LIKE '`+providerEchoModelPattern+`'
		   AND logs.id > ?
		 ORDER BY logs.id
		 LIMIT ?`,
		afterID, batchSize,
	)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("usage: query provider-echo model rows: %w", err)
	}
	defer rows.Close()

	type targetRow struct {
		ID              int64
		InputTokens     int64
		OutputTokens    int64
		ReasoningTokens int64
		CachedTokens    int64
		TotalTokens     int64
		Compression     string
		ContentInput    []byte
		LegacyInput     string
		RecoveredModel  string
	}
	batch := make([]targetRow, 0, batchSize)
	for rows.Next() {
		var row targetRow
		if err := rows.Scan(
			&row.ID,
			&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
			&row.CachedTokens, &row.TotalTokens,
			&row.Compression,
			&row.ContentInput,
			&row.LegacyInput,
		); err != nil {
			return 0, 0, 0, fmt.Errorf("usage: scan provider-echo model row: %w", err)
		}
		if row.ID > lastID {
			lastID = row.ID
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("usage: iterate provider-echo model rows: %w", err)
	}
	if len(batch) == 0 {
		return 0, 0, 0, nil
	}

	for i := range batch {
		row := &batch[i]
		model, ok := recoverModelFromStoredInput(row.Compression, row.ContentInput, row.LegacyInput)
		if !ok {
			unrecoverable++
			continue
		}
		row.RecoveredModel = model
	}

	// Backfill is a DB maintenance task, not bound to any request lifecycle. Use the
	// root context so the transaction is governed only by DB errors.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, unrecoverable, lastID, fmt.Errorf("usage: begin model backfill tx: %w", err)
	}

	for i := range batch {
		row := &batch[i]
		if row.RecoveredModel == "" {
			continue
		}
		cost := CalculateCostV2(row.RecoveredModel, TokenStats{
			InputTokens:     row.InputTokens,
			OutputTokens:    row.OutputTokens,
			ReasoningTokens: row.ReasoningTokens,
			CachedTokens:    row.CachedTokens,
			TotalTokens:     row.TotalTokens,
		})
		if _, errExec := tx.Exec(
			`UPDATE request_logs SET model = ?, cost = ? WHERE id = ?`,
			row.RecoveredModel, cost, row.ID,
		); errExec != nil {
			_ = tx.Rollback()
			return 0, unrecoverable, lastID, fmt.Errorf("usage: update request_logs model: %w", errExec)
		}
		rewritten++
	}

	if err := tx.Commit(); err != nil {
		return 0, unrecoverable, lastID, fmt.Errorf("usage: commit model backfill: %w", err)
	}
	return rewritten, unrecoverable, lastID, nil
}

// recoverModelFromStoredInput extracts the original client-requested model from the
// stored request body. It tries the compressed content row first, then the legacy
// uncompressed input_content column. Returns the clean model and true only when a
// usable, non-echo model name was recovered.
func recoverModelFromStoredInput(compression string, contentInput []byte, legacyInput string) (string, bool) {
	var raw string
	if len(contentInput) > 0 {
		decoded, err := decompressLogContent(compression, contentInput)
		if err != nil {
			return "", false
		}
		raw = decoded
	} else if strings.TrimSpace(legacyInput) != "" {
		raw = legacyInput
	} else {
		return "", false
	}

	model := extractModelFromPayload(raw)
	if !isUsableCleanModel(model) {
		return "", false
	}
	return model, true
}

// extractModelFromPayload reads the "model" field from a JSON request body. It
// tolerates leading whitespace; on any parse failure it returns "".
func extractModelFromPayload(payload string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// isUsableCleanModel reports whether a recovered model string is safe to write back:
// non-empty, not itself a provider-echo path, and not the "unknown" placeholder.
func isUsableCleanModel(model string) bool {
	if model == "" {
		return false
	}
	if isProviderEchoModel(model) {
		return false
	}
	if model == "unknown" {
		return false
	}
	return true
}

// isProviderEchoModel matches the upstream provider-internal path shape that this
// backfill repairs. Kept conservative on purpose: legitimate slash model names from
// other providers are not matched.
func isProviderEchoModel(model string) bool {
	return strings.HasPrefix(model, "accounts/") && strings.Contains(model, "/models/")
}
