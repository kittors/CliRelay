package usage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// insertProviderEchoRequestLog inserts a request_logs row with a bad provider-echo
// model and returns its id. Token columns are populated so cost recompute is testable.
func insertProviderEchoRequestLog(t *testing.T, db *sql.DB, model string, inputTokens int64) int64 {
	t.Helper()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp, "sk-test", "tester", model, "source", "channel", "auth-1",
		0, 123, 45, inputTokens, 20, 0, 0, inputTokens+20, 0,
	)
	if err != nil {
		t.Fatalf("insert request_log with model %q: %v", model, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func insertRequestLogContentRow(t *testing.T, db *sql.DB, logID int64, inputJSON string) {
	t.Helper()
	compressed, err := compressLogContent(inputJSON)
	if err != nil {
		t.Fatalf("compress content: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content, detail_content)
		 VALUES (?, ?, 'zstd', ?, X'', X'')`,
		logID, time.Now().UTC().Format(time.RFC3339Nano), compressed,
	); err != nil {
		t.Fatalf("insert request_log_content row: %v", err)
	}
}

func upsertTestModelPricing(t *testing.T, modelID string, inputPricePerMillion float64) {
	t.Helper()
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               modelID,
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  inputPricePerMillion,
		OutputPricePerMillion: 0,
		UpdatedAt:             time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertModelConfig(%s): %v", modelID, err)
	}
}

// TestBackfillRequestLogModelNamesRecoversFromCompressedContent verifies the main
// path: a row with a provider-echo model and a compressed request_log_content row
// gets its model recovered from the stored request body and cost recomputed.
func TestBackfillRequestLogModelNamesRecoversFromCompressedContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	// Register pricing for the clean model so cost recompute is verifiable.
	upsertTestModelPricing(t, "glm-5.2", 1.0) // $1 per million input tokens

	id := insertProviderEchoRequestLog(t, db, "accounts/fireworks/models/glm-5p2", 1000)
	insertRequestLogContentRow(t, db, id, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

	backfillRequestLogModelNames(db)

	var model string
	var cost float64
	if err := db.QueryRow("SELECT model, cost FROM request_logs WHERE id = ?", id).Scan(&model, &cost); err != nil {
		t.Fatalf("query repaired row: %v", err)
	}
	if model != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2", model)
	}
	// 1000 input tokens @ $1/M = 0.001
	if cost <= 0 {
		t.Fatalf("cost = %v, expected recompute to a positive value for glm-5.2", cost)
	}
}

// TestBackfillRequestLogModelNamesRecoversFromLegacyInput verifies the fallback to
// the legacy uncompressed request_logs.input_content column for older rows.
func TestBackfillRequestLogModelNamesRecoversFromLegacyInput(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	id := insertProviderEchoRequestLog(t, db, "accounts/fireworks/models/glm-5p2", 500)
	// Write the request body into the legacy input_content column directly (no
	// request_log_content row).
	if _, err := db.Exec(
		`UPDATE request_logs SET input_content = ? WHERE id = ?`,
		`{"model":"glm-5.2","input":"ping"}`, id,
	); err != nil {
		t.Fatalf("set legacy input_content: %v", err)
	}

	backfillRequestLogModelNames(db)

	var model string
	if err := db.QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query repaired row: %v", err)
	}
	if model != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2 (legacy recovery)", model)
	}
}

// TestBackfillRequestLogModelNamesSkipsUnrecoverableRows verifies that a bad row
// with no stored request body is left unchanged (not silently fabricated).
func TestBackfillRequestLogModelNamesSkipsUnrecoverableRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	id := insertProviderEchoRequestLog(t, db, "accounts/fireworks/models/glm-5p2", 100)
	// No content row, no legacy input_content.

	backfillRequestLogModelNames(db)

	var model string
	if err := db.QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if model != "accounts/fireworks/models/glm-5p2" {
		t.Fatalf("unrecoverable row model = %q, must be left unchanged", model)
	}
}

// TestBackfillRequestLogModelNamesLeavesLegitimateSlashModelsUntouched verifies
// that legitimate slash model names (e.g. meta/llama-3.1-8b-instruct) are not
// treated as bad provider-echo paths and are left alone.
func TestBackfillRequestLogModelNamesLeavesLegitimateSlashModelsUntouched(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	id := insertProviderEchoRequestLog(t, db, "meta/llama-3.1-8b-instruct", 100)
	insertRequestLogContentRow(t, db, id, `{"model":"meta/llama-3.1-8b-instruct","messages":[]}`)

	backfillRequestLogModelNames(db)

	var model string
	if err := db.QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if model != "meta/llama-3.1-8b-instruct" {
		t.Fatalf("legitimate slash model changed to %q, must stay meta/llama-3.1-8b-instruct", model)
	}
}

// TestBackfillRequestLogModelNamesIsIdempotent verifies that re-running the
// backfill after a successful repair does not reprocess or alter rows.
func TestBackfillRequestLogModelNamesIsIdempotent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()

	id := insertProviderEchoRequestLog(t, db, "accounts/fireworks/models/glm-5p2", 800)
	insertRequestLogContentRow(t, db, id, `{"model":"glm-5.2","messages":[]}`)

	backfillRequestLogModelNames(db)
	var model string
	if err := db.QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query row after first backfill: %v", err)
	}
	// Run again — must be a no-op for this row.
	backfillRequestLogModelNames(db)

	if err := db.QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query row after second backfill: %v", err)
	}
	if model != "glm-5.2" {
		t.Fatalf("model = %q after idempotent re-run, want glm-5.2", model)
	}
}

// TestRecoverModelFromStoredInputGuards verifies the recovery helper rejects
// recovered values that are themselves echo paths or placeholders.
func TestRecoverModelFromStoredInputGuards(t *testing.T) {
	cases := []struct {
		name        string
		compression string
		content     string
		legacy      string
		wantOK      bool
		wantModel   string
	}{
		{name: "clean from legacy", legacy: `{"model":"glm-5.2"}`, wantOK: true, wantModel: "glm-5.2"},
		{name: "echo recovered is rejected", legacy: `{"model":"accounts/fireworks/models/glm-5p2"}`, wantOK: false},
		{name: "unknown placeholder rejected", legacy: `{"model":"unknown"}`, wantOK: false},
		{name: "empty body", legacy: "", wantOK: false},
		{name: "non-json body", legacy: "not json", wantOK: false},
		{name: "missing model field", legacy: `{"foo":"bar"}`, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var contentBytes []byte
			if tc.content != "" {
				c, err := compressLogContent(tc.content)
				if err != nil {
					t.Fatalf("compress: %v", err)
				}
				contentBytes = c
			}
			gotModel, gotOK := recoverModelFromStoredInput(tc.compression, contentBytes, tc.legacy)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (model=%q)", gotOK, tc.wantOK, gotModel)
			}
			if gotOK && gotModel != tc.wantModel {
				t.Fatalf("model = %q, want %q", gotModel, tc.wantModel)
			}
		})
	}
}
