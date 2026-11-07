package usage

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

type guardMonitorRow struct {
	tenant   string
	keyID    string
	model    string
	kind     string
	failed   bool
	cost     float64
	at       time.Time
	channel  string
	streamed bool
}

func seedGuardMonitorRows(t *testing.T, rows ...guardMonitorRow) {
	t.Helper()
	for _, row := range rows {
		tenant := row.tenant
		if tenant == "" {
			tenant = systemTenantID
		}
		failed := 0
		if row.failed || row.kind != "" {
			failed = 1
		}
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, api_key, api_key_id, model, source, channel_name, failed, streaming, total_tokens, cost, guard_kind)
			VALUES (?, ?, 'masked', ?, ?, 'test', ?, ?, 0, 10, ?, ?)`,
			tenant, row.at.UTC().Format(time.RFC3339Nano), row.keyID, row.model, row.channel, failed, row.cost, row.kind,
		); err != nil {
			t.Fatalf("insert request log: %v", err)
		}
	}
}

func TestMonitorCountsWhatLoopProtectionEnded(t *testing.T) {
	initMonitorTestDB(t)
	in := monitorTestNow.Add(-2 * time.Hour)
	before := monitorTestNow.Add(-30 * time.Hour) // outside the 24h window
	seedGuardMonitorRows(t,
		// priced successes: gpt-x averages 0.4, claude-y averages 1.0
		guardMonitorRow{keyID: "key-a", model: "gpt-x", cost: 0.3, at: in},
		guardMonitorRow{keyID: "key-a", model: "gpt-x", cost: 0.5, at: in},
		guardMonitorRow{keyID: "key-a", model: "claude-y", cost: 1.0, at: in},
		// failures and zero-cost rows do not enter the averages
		guardMonitorRow{keyID: "key-a", model: "gpt-x", failed: true, cost: 9, at: in},
		guardMonitorRow{keyID: "key-a", model: "gpt-x", cost: 0, at: in},
		// loop protection
		guardMonitorRow{keyID: "key-a", model: "gpt-x", kind: "tool_loop", at: in},
		guardMonitorRow{keyID: "key-a", model: "gpt-x", kind: "tool_loop", at: in},
		guardMonitorRow{keyID: "key-a", model: "claude-y", kind: "tool_loop", at: in},
		guardMonitorRow{keyID: "key-b", model: "claude-y", kind: "reply_loop", cost: 0.7, at: in},
		guardMonitorRow{keyID: "key-b", model: "no-price-model", kind: "tool_loop", at: in},
		// outside the window, or another tenant's
		guardMonitorRow{keyID: "key-a", model: "gpt-x", kind: "tool_loop", at: before},
		guardMonitorRow{tenant: monitorOtherTenant, keyID: "key-a", model: "gpt-x", kind: "reply_loop", at: in},
	)

	overview, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID}, "24h", monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	got := overview.LoopGuard
	if got.ReplyLoops != 1 || got.ToolLoops != 4 {
		t.Fatalf("counts = %+v, want 1 reply loop and 4 tool loops", got)
	}
	// 2 x gpt-x at 0.4 + 1 x claude-y at 1.0; the unpriced model adds nothing,
	// and the cut reply is not estimated.
	if want := 2*0.4 + 1.0; math.Abs(got.EstimatedSavedCost-want) > 1e-9 {
		t.Fatalf("estimated saved cost = %v, want %v", got.EstimatedSavedCost, want)
	}

	// The section is part of the payload under its name, always, so the panel
	// can tell a backend that has it from one that has not.
	payload, _ := json.Marshal(overview)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["loop_guard"]; !ok {
		t.Fatal("no loop_guard in the monitor payload")
	}

	t.Run("follows the consumer filter", func(t *testing.T) {
		overview, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID, APIKeyIDs: []string{"key-b"}}, "24h", monitorTestNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := overview.LoopGuard; got.ReplyLoops != 1 || got.ToolLoops != 1 || got.EstimatedSavedCost != 0 {
			t.Fatalf("key-b counts = %+v (its own successes are none, so nothing is estimated)", got)
		}
	})
	t.Run("follows the model filter", func(t *testing.T) {
		overview, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID, Models: []string{"claude-y"}}, "24h", monitorTestNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := overview.LoopGuard; got.ReplyLoops != 1 || got.ToolLoops != 1 || math.Abs(got.EstimatedSavedCost-1.0) > 1e-9 {
			t.Fatalf("claude-y counts = %+v", got)
		}
	})
	t.Run("another tenant sees its own", func(t *testing.T) {
		overview, err := QueryMonitorOverview(MonitorFilter{TenantID: monitorOtherTenant}, "24h", monitorTestNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := overview.LoopGuard; got.ReplyLoops != 1 || got.ToolLoops != 0 {
			t.Fatalf("other tenant counts = %+v", got)
		}
	})
}

func TestMonitorWithoutLoopProtectionRowsReportsZeros(t *testing.T) {
	initMonitorTestDB(t)
	seedGuardMonitorRows(t, guardMonitorRow{keyID: "key-a", model: "gpt-x", cost: 0.3, at: monitorTestNow.Add(-time.Hour)})
	overview, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID}, "24h", monitorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := overview.LoopGuard; got != (MonitorLoopGuard{}) {
		t.Fatalf("loop guard = %+v, want zeros", got)
	}
}
