package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// The settled hook tells the budget reservations when a request's usage is
// visible to the checks (committed) or never will be (dropped). A spooled
// entry is neither yet: its reservation runs out on its own.

type settledCall struct {
	requestID string
	billed    bool
	cost      float64
	tokens    int64
}

func captureSettled(t *testing.T) *[]settledCall {
	t.Helper()
	calls := &[]settledCall{}
	SetRequestLogSettledHook(func(requestID string, billed bool, cost float64, tokens int64) {
		*calls = append(*calls, settledCall{requestID, billed, cost, tokens})
	})
	t.Cleanup(func() { SetRequestLogSettledHook(nil) })
	return calls
}

func settledTestEntry(requestID, model string) RequestLogEntry {
	entry := resilienceTestEntry(requestID+"-idem", model, 1000, time.Now())
	entry.RequestID = requestID
	return entry
}

func TestACommittedRequestLogIsSettledWithWhatItWasBilled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID: "settled-priced-model", Enabled: true, PricingMode: "token",
		InputPricePerMillion: 10, OutputPricePerMillion: 20,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}
	calls := captureSettled(t)

	InsertRequestLog(settledTestEntry("req-1", "settled-priced-model"))
	var stored float64
	if err := getDB().QueryRow(`SELECT cost FROM request_logs`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored <= 0 {
		t.Fatalf("stored cost = %v, want the priced model billed", stored)
	}
	if len(*calls) != 1 || (*calls)[0] != (settledCall{"req-1", true, stored, 1001}) {
		t.Fatalf("settled = %+v, want one billed call carrying the stored cost %v", *calls, stored)
	}

	// A second delivery of the same record finds it stored: still settled,
	// since the usage is in the database.
	InsertRequestLog(settledTestEntry("req-1", "settled-priced-model"))
	if len(*calls) != 2 || !(*calls)[1].billed {
		t.Fatalf("settled after a duplicate = %+v", *calls)
	}
}

func TestAFailedRequestLogIsSettledUnbilled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	calls := captureSettled(t)
	entry := settledTestEntry("req-failed", "model-a")
	entry.Failed = true
	InsertRequestLog(entry)
	if len(*calls) != 1 || (*calls)[0].requestID != "req-failed" || (*calls)[0].billed {
		t.Fatalf("settled = %+v, want one unbilled call", *calls)
	}
}

func TestADroppedRequestLogIsSettledUnbilled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	calls := captureSettled(t)
	writeRequestLogAttemptFunc = func(RequestLogEntry, time.Duration) (requestLogWriteResult, error) {
		return requestLogWriteResult{}, errors.New("CHECK constraint failed")
	}
	InsertRequestLog(settledTestEntry("req-dropped", "model-a"))
	if len(*calls) != 1 || (*calls)[0] != (settledCall{requestID: "req-dropped", tokens: 1001}) {
		t.Fatalf("settled = %+v, want one unbilled call for the dropped entry", *calls)
	}
}

func TestASpooledRequestLogIsNotSettledYet(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	useUsageSpoolForTest(t, "", 0)
	calls := captureSettled(t)
	usageDBHealth.markDown(errFakeAdminShutdown)
	InsertRequestLog(settledTestEntry("req-spooled", "model-a"))
	if len(*calls) != 0 {
		t.Fatalf("settled = %+v, want nothing while the entry waits in the spool", *calls)
	}
}

func TestARecordCarriesItsRequestIDToTheSettledHook(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	calls := captureSettled(t)
	NewRequestStatistics().Record(context.Background(), coreusage.Record{
		IdempotencyKey: coreusage.NewIdempotencyKey(),
		RequestID:      " req-record ",
		APIKey:         "sk-resilience-XXXX",
		Model:          "model-a",
		RequestedAt:    time.Now(),
		Detail:         coreusage.Detail{InputTokens: 5, OutputTokens: 5},
	})
	if len(*calls) != 1 || (*calls)[0].requestID != "req-record" {
		t.Fatalf("settled = %+v, want the record's request id", *calls)
	}
	// An entry without a request id (written outside a gateway request) is
	// not reported.
	InsertRequestLog(resilienceTestEntry("no-request", "model-a", 1, time.Now()))
	if len(*calls) != 1 {
		t.Fatalf("settled = %+v, want nothing more", *calls)
	}
}
