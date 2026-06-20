package usage

import (
	"fmt"
	"strings"
)

// LogContentResult holds the content detail for a single log entry.
type LogContentResult struct {
	ID            int64  `json:"id"`
	InputContent  string `json:"input_content"`
	OutputContent string `json:"output_content"`
	Model         string `json:"model"`
}

// LogContentPartResult holds one side (input/output) of the content detail for a single log entry.
// It is used to avoid decompressing/transferring both large blobs when the UI only needs one tab.
type LogContentPartResult struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	Model   string `json:"model"`
	Part    string `json:"part"`
}

func normalizeLogContentPart(part string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(part)) {
	case "input":
		return "input", nil
	case "output":
		return "output", nil
	case "details":
		return "details", nil
	default:
		return "", fmt.Errorf("usage: invalid content part %q", part)
	}
}

// QueryLogContent retrieves the stored request/response content for a single log entry.
func QueryLogContent(id int64) (LogContentResult, error) {
	db := getReadDB()
	if db == nil {
		return LogContentResult{}, fmt.Errorf("usage: database not initialised")
	}

	result, err := queryCompressedLogContent(
		db,
		`SELECT logs.id, logs.model, content.compression, content.input_content, content.output_content
		 FROM request_logs logs
		 JOIN request_log_content content ON content.log_id = logs.id
		 WHERE logs.id = ?`,
		id,
	)
	if err == nil {
		return result, nil
	}

	var fallback LogContentResult
	err = db.QueryRow(
		"SELECT id, model, input_content, output_content FROM request_logs WHERE id = ?", id,
	).Scan(&fallback.ID, &fallback.Model, &fallback.InputContent, &fallback.OutputContent)
	if err != nil {
		return LogContentResult{}, fmt.Errorf("usage: query log content: %w", err)
	}
	return fallback, nil
}

// QueryLogContentPart retrieves only one side (input/output) of the stored request/response content
// for a single log entry. This avoids decompressing/transferring both blobs for the UI.
func QueryLogContentPart(id int64, part string) (LogContentPartResult, error) {
	db := getReadDB()
	if db == nil {
		return LogContentPartResult{}, fmt.Errorf("usage: database not initialised")
	}

	part, err := normalizeLogContentPart(part)
	if err != nil {
		return LogContentPartResult{}, err
	}

	column := "input_content"
	if part == "output" {
		column = "output_content"
	} else if part == "details" {
		column = "detail_content"
	}

	result, err := queryCompressedLogContentPart(
		db,
		part,
		fmt.Sprintf(
			`SELECT logs.id, logs.model, content.compression, content.%s
			 FROM request_logs logs
			 JOIN request_log_content content ON content.log_id = logs.id
			 WHERE logs.id = ?`,
			column,
		),
		id,
	)
	if err == nil {
		return result, nil
	}
	if part == "details" {
		var fallback LogContentPartResult
		fallback.Part = part
		err = db.QueryRow("SELECT id, model FROM request_logs WHERE id = ?", id).Scan(&fallback.ID, &fallback.Model)
		if err != nil {
			return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
		}
		return fallback, nil
	}

	var fallback LogContentPartResult
	fallback.Part = part
	err = db.QueryRow(
		fmt.Sprintf("SELECT id, model, %s FROM request_logs WHERE id = ?", column),
		id,
	).Scan(&fallback.ID, &fallback.Model, &fallback.Content)
	if err != nil {
		return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
	}
	return fallback, nil
}

// QueryLogContentForKey retrieves log content for a single entry, but only if it belongs to the given API key.
// This is used by the public endpoint to ensure users can only access their own logs.
func QueryLogContentForKey(id int64, apiKey string) (LogContentResult, error) {
	db := getReadDB()
	if db == nil {
		return LogContentResult{}, fmt.Errorf("usage: database not initialised")
	}
	clause, args := buildSingleAPIKeySelectorClause(apiKey)
	predicate := strings.TrimPrefix(clause, " WHERE ")
	queryArgs := append([]interface{}{id}, args...)

	result, err := queryCompressedLogContent(
		db,
		`SELECT logs.id, logs.model, content.compression, content.input_content, content.output_content
		 FROM request_logs logs
		 JOIN request_log_content content ON content.log_id = logs.id
		 WHERE logs.id = ? AND `+predicate,
		queryArgs...,
	)
	if err == nil {
		return result, nil
	}

	var fallback LogContentResult
	err = db.QueryRow(
		"SELECT id, model, input_content, output_content FROM request_logs WHERE id = ? AND "+predicate,
		queryArgs...,
	).Scan(&fallback.ID, &fallback.Model, &fallback.InputContent, &fallback.OutputContent)
	if err != nil {
		return LogContentResult{}, fmt.Errorf("usage: query log content: %w", err)
	}
	return fallback, nil
}

// QueryLogContentPartForKey retrieves only one side (input/output) of the stored request/response content
// for a single entry, but only if it belongs to the given API key.
func QueryLogContentPartForKey(id int64, apiKey string, part string) (LogContentPartResult, error) {
	db := getReadDB()
	if db == nil {
		return LogContentPartResult{}, fmt.Errorf("usage: database not initialised")
	}

	part, err := normalizeLogContentPart(part)
	if err != nil {
		return LogContentPartResult{}, err
	}

	column := "input_content"
	if part == "output" {
		column = "output_content"
	} else if part == "details" {
		column = "detail_content"
	}
	clause, args := buildSingleAPIKeySelectorClause(apiKey)
	predicate := strings.TrimPrefix(clause, " WHERE ")
	queryArgs := append([]interface{}{id}, args...)

	result, err := queryCompressedLogContentPart(
		db,
		part,
		fmt.Sprintf(
			`SELECT logs.id, logs.model, content.compression, content.%s
			 FROM request_logs logs
			 JOIN request_log_content content ON content.log_id = logs.id
			 WHERE logs.id = ? AND %s`,
			column,
			predicate,
		),
		queryArgs...,
	)
	if err == nil {
		return result, nil
	}
	if part == "details" {
		var fallback LogContentPartResult
		fallback.Part = part
		err = db.QueryRow("SELECT id, model FROM request_logs WHERE id = ? AND "+predicate, queryArgs...).Scan(&fallback.ID, &fallback.Model)
		if err != nil {
			return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
		}
		return fallback, nil
	}

	var fallback LogContentPartResult
	fallback.Part = part
	err = db.QueryRow(
		fmt.Sprintf("SELECT id, model, %s FROM request_logs WHERE id = ? AND %s", column, predicate),
		queryArgs...,
	).Scan(&fallback.ID, &fallback.Model, &fallback.Content)
	if err != nil {
		return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
	}
	return fallback, nil
}
