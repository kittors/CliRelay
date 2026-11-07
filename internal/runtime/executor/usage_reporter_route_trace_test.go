package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func awaitUsageRecord(t *testing.T, records <-chan coreusage.Record, model string, within time.Duration) (coreusage.Record, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case record := <-records:
			if record.Model == model {
				return record, true
			}
		case <-deadline:
			return coreusage.Record{}, false
		}
	}
}

// An attempt's record waits for the scheduler to settle the attempt, and its
// request-log detail is built then: it carries the route trace as the request
// moved on, not as the attempt left it.
func TestUsageReporterHoldsItsRecordUntilTheAttemptSettles(t *testing.T) {
	const model = "route-trace-held-record-test"
	plugin := &tenantUsageCapturePlugin{records: make(chan coreusage.Record, 8)}
	coreusage.RegisterPlugin(plugin)

	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	diagnostics.EnsureGin(ginCtx, "req-held-record")
	ctx := context.WithValue(ginCtx.Request.Context(), util.ContextKeyGin, ginCtx)
	rec := routetrace.FromContext(ctx)
	first := rec.BeginAttempt(routetrace.AttemptInfo{AuthID: "acct-first", Provider: "codex"})
	attemptCtx := first.Context(ctx)

	reporter := newUsageReporter(attemptCtx, "codex", model, "", nil, false)
	reporter.publishFailure(attemptCtx)
	if _, published := awaitUsageRecord(t, plugin.records, model, 50*time.Millisecond); published {
		t.Fatal("the record of a running attempt must wait for the attempt")
	}
	first.Finish(routetrace.Outcome{Outcome: routetrace.OutcomeFailed, Status: http.StatusTooManyRequests})
	if _, published := awaitUsageRecord(t, plugin.records, model, 50*time.Millisecond); published {
		t.Fatal("a failed attempt's record must wait for the scheduler's next decision")
	}
	rec.BeginAttempt(routetrace.AttemptInfo{AuthID: "acct-next", Provider: "codex"})

	record, published := awaitUsageRecord(t, plugin.records, model, 2*time.Second)
	if !published {
		t.Fatal("the record must go out once the request moves on")
	}
	if !record.Failed {
		t.Fatal("the held record keeps the outcome it was emitted with")
	}
	var detail struct {
		Diagnostic struct {
			RouteTrace routetrace.Trace `json:"route_trace"`
		} `json:"diagnostic"`
	}
	if err := json.Unmarshal([]byte(record.DetailContent), &detail); err != nil {
		t.Fatalf("detail content: %v (%q)", err, record.DetailContent)
	}
	steps := detail.Diagnostic.RouteTrace.Steps
	if len(steps) != 2 || steps[0].AuthID != "acct-first" || steps[0].Status != http.StatusTooManyRequests || steps[1].AuthID != "acct-next" {
		t.Fatalf("route trace in the detail = %+v, want the failed attempt and the next one", steps)
	}
}

// Without a traced attempt in the context a record goes out at once.
func TestUsageReporterPublishesAtOnceWithoutAnAttempt(t *testing.T) {
	const model = "route-trace-untraced-record-test"
	plugin := &tenantUsageCapturePlugin{records: make(chan coreusage.Record, 8)}
	coreusage.RegisterPlugin(plugin)

	ctx := routetrace.WithRecorder(context.Background(), routetrace.New())
	reporter := newUsageReporter(ctx, "codex", model, "", nil, false)
	reporter.publishFailure(ctx)
	if _, published := awaitUsageRecord(t, plugin.records, model, 2*time.Second); !published {
		t.Fatal("a record outside an attempt must go out at once")
	}
}
