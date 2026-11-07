package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	"github.com/tidwall/gjson"
)

type guardRecordCapture struct {
	records chan coreusage.Record
}

func (p *guardRecordCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	select {
	case p.records <- record:
	default:
	}
}

// capturedGuardRecord registers a plugin and returns a function that waits for
// the record of the given model, which keeps the tests apart: the usage manager
// is process-wide.
func capturedGuardRecord(t *testing.T) func(model string) coreusage.Record {
	t.Helper()
	plugin := &guardRecordCapture{records: make(chan coreusage.Record, 64)}
	coreusage.RegisterPlugin(plugin)
	return func(model string) coreusage.Record {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case record := <-plugin.records:
				if record.Model == model {
					return record
				}
			case <-deadline:
				t.Fatalf("no usage record for model %q", model)
			}
		}
	}
}

// loopGuardRequest returns the handler-side context of a request: the gin
// context with a diagnostic and the caller's API key.
func loopGuardRequest(t *testing.T) (context.Context, *diagnostics.RequestDiagnostic) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	c.Request.Header.Set("User-Agent", "loop-guard-test")
	d := diagnostics.EnsureGin(c, "req-guard")
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, c)
	ctx = context.WithValue(ctx, util.ContextKeyAPIKey, "sk-test-secret-key-0001")
	return ctx, d
}

func TestGuardOutcome(t *testing.T) {
	cutCtx := func(kind loopdetect.Kind) context.Context {
		ctx, mark := loopdetect.WithMark(context.Background())
		mark.Set(kind, "ended")
		return ctx
	}
	unsetCtx, _ := loopdetect.WithMark(context.Background())

	for name, tc := range map[string]struct {
		ctx        context.Context
		reporter   string // guardKind preset on the reporter
		failed     bool
		wantFailed bool
		wantKind   string
	}{
		"nothing happened":                     {ctx: context.Background(), wantFailed: false},
		"an upstream failure stays that":       {ctx: context.Background(), failed: true, wantFailed: true},
		"a mark nobody set":                    {ctx: unsetCtx, wantFailed: false},
		"a cut reply is a failure of its kind": {ctx: cutCtx(loopdetect.KindReplyLoop), wantFailed: true, wantKind: "reply_loop"},
		"a cut reply that also failed":         {ctx: cutCtx(loopdetect.KindReplyLoop), failed: true, wantFailed: true, wantKind: "reply_loop"},
		"a request the gateway answered":       {ctx: context.Background(), reporter: "tool_loop", wantFailed: true, wantKind: "tool_loop"},
		"the reporter's own kind wins":         {ctx: cutCtx(loopdetect.KindReplyLoop), reporter: "tool_loop", wantFailed: true, wantKind: "tool_loop"},
		"no context at all":                    {ctx: nil, wantFailed: false},
	} {
		t.Run(name, func(t *testing.T) {
			reporter := &usageReporter{guardKind: tc.reporter}
			failed, kind := reporter.guardOutcome(tc.ctx, tc.failed)
			if failed != tc.wantFailed || kind != tc.wantKind {
				t.Fatalf("guardOutcome() = (%v, %q), want (%v, %q)", failed, kind, tc.wantFailed, tc.wantKind)
			}
		})
	}
}

// The executor sees only a cancelled upstream request when the handler cuts a
// reply. The mark on the context is how the request is still filed as the
// failure it was, with whatever the upstream had reported until then.
func TestUsageReporterFilesACutReplyAsAGuardFailure(t *testing.T) {
	record := capturedGuardRecord(t)
	run := func(model string, cut bool, publish func(r *usageReporter, ctx context.Context)) coreusage.Record {
		ctx, mark := loopdetect.WithMark(context.Background())
		reporter := newUsageReporter(ctx, "claude", model, "", nil, true)
		if cut {
			mark.Set(loopdetect.KindReplyLoop, "Reply stopped by the gateway's loop protection")
		}
		publish(reporter, ctx)
		return record(model)
	}

	t.Run("published at the end of the stream", func(t *testing.T) {
		got := run("guard-cut-ensure", true, func(r *usageReporter, ctx context.Context) { r.ensurePublished(ctx) })
		if !got.Failed || got.GuardKind != "reply_loop" || !got.Streaming {
			t.Fatalf("record = failed %v kind %q streaming %v", got.Failed, got.GuardKind, got.Streaming)
		}
	})
	t.Run("the tokens the upstream reported before the cut are kept", func(t *testing.T) {
		got := run("guard-cut-tokens", true, func(r *usageReporter, ctx context.Context) {
			r.publish(ctx, coreusage.Detail{InputTokens: 120, OutputTokens: 800})
		})
		if !got.Failed || got.GuardKind != "reply_loop" {
			t.Fatalf("record = failed %v kind %q", got.Failed, got.GuardKind)
		}
		if got.Detail.InputTokens != 120 || got.Detail.OutputTokens != 800 || got.Detail.TotalTokens != 920 {
			t.Fatalf("tokens = %+v", got.Detail)
		}
	})
	t.Run("a request nobody cut is untouched", func(t *testing.T) {
		got := run("guard-uncut", false, func(r *usageReporter, ctx context.Context) {
			r.publish(ctx, coreusage.Detail{InputTokens: 5, OutputTokens: 7})
		})
		if got.Failed || got.GuardKind != "" {
			t.Fatalf("record = failed %v kind %q", got.Failed, got.GuardKind)
		}
	})
	t.Run("a cut with no usage still files the request", func(t *testing.T) {
		// publish returns early for a request with no tokens and no failure; a cut one is a failure.
		got := run("guard-cut-empty", true, func(r *usageReporter, ctx context.Context) { r.publish(ctx, coreusage.Detail{}) })
		if !got.Failed || got.GuardKind != "reply_loop" {
			t.Fatalf("record = failed %v kind %q", got.Failed, got.GuardKind)
		}
	})
}

func TestLoopGuardObserverFilesAToolLoopWithoutAnUpstream(t *testing.T) {
	record := capturedGuardRecord(t)
	ctx, d := loopGuardRequest(t)
	observer := LoopGuardObserver{ToolSource: "api_key"}
	observer.ToolLoopAnswered(ctx, loopdetect.ToolAnswered{
		Limits: loopdetect.ToolLimits{Repeats: 8},
		Loop: loopdetect.ToolLoop{
			Turns: 9, Repeats: 8, Items: 40, Read: 16,
			Calls: []loopdetect.ToolCall{{Name: "Read", Arguments: `{"file_path":"/a.go"}`}},
		},
		Model:    "guard-tool-model",
		Protocol: loopdetect.ProtocolClaude,
		Stream:   true,
	})

	got := record("guard-tool-model")
	if !got.Failed || got.GuardKind != "tool_loop" || !got.Streaming {
		t.Fatalf("record = failed %v kind %q streaming %v", got.Failed, got.GuardKind, got.Streaming)
	}
	if got.Detail.TotalTokens != 0 || got.Detail.InputTokens != 0 {
		t.Fatalf("a request sent upstream nowhere has tokens: %+v", got.Detail)
	}
	if got.AuthID != "" || got.AuthIndex != "" || got.ChannelName != "" {
		t.Fatalf("a request no account served names one: auth %q/%q channel %q", got.AuthID, got.AuthIndex, got.ChannelName)
	}
	if got.Source != "" {
		t.Fatalf("source = %q; the caller's own API key must not become the source of a request nothing served", got.Source)
	}
	if got.RequestID == "" && got.APIIdentifier == "" {
		t.Fatalf("the record carries no request identity: %+v", got)
	}
	if !strings.Contains(got.OutputContent, "Stop tool-call loops") {
		t.Fatalf("the log does not say what the client was told: %q", got.OutputContent)
	}
	if code := gjson.Get(got.OutputContent, "error.code").String(); code != "tool_loop_answered" {
		t.Fatalf("output error code = %q in %q", code, got.OutputContent)
	}

	// The same finding is in the diagnostics, which the record's detail carries.
	snapshot := d.Snapshot().LoopGuard
	if snapshot == nil || snapshot.Kind != diagnostics.LoopGuardToolLoop || snapshot.Source != "api_key" ||
		snapshot.Repeats != 8 || snapshot.Turns != 9 || len(snapshot.Calls) != 1 || snapshot.Calls[0].Name != "Read" ||
		snapshot.ConversationItems != 40 || snapshot.ItemsRead != 16 || snapshot.Message == "" {
		t.Fatalf("diagnostics = %+v", snapshot)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(got.DetailContent), &detail); err != nil {
		t.Fatalf("detail content is not JSON: %v", err)
	}
	if kind := gjson.Get(got.DetailContent, "diagnostic.loop_guard.kind").String(); kind != "tool_loop" {
		t.Fatalf("detail diagnostic loop_guard.kind = %q in %s", kind, got.DetailContent)
	}
}

func TestLoopGuardObserverRecordsACutReplyInTheDiagnostics(t *testing.T) {
	ctx, d := loopGuardRequest(t)
	LoopGuardObserver{ReplySource: "tenant"}.ReplyCut(ctx, loopdetect.ReplyCut{
		Limits: loopdetect.ReplyLimits{Window: 256, Distinct: 24},
		Trip: loopdetect.ReplyTrip{
			Reasoning: true, Units: []string{"Okay.", "Hmm."}, Window: 256, Distinct: 3,
			UnitsRead: 300, Chunks: 412, Bytes: 51234,
		},
		Model: "guard-reply-model",
	})
	got := d.Snapshot().LoopGuard
	if got == nil || got.Kind != diagnostics.LoopGuardReplyLoop || got.Source != "tenant" ||
		got.Window != 256 || got.MaxDistinct != 24 || got.Distinct != 3 || !got.Reasoning ||
		got.UnitsRead != 300 || got.CutAtChunk != 412 || got.CutAtBytes != 51234 ||
		len(got.Units) != 2 || !strings.Contains(got.Message, "loop protection") {
		t.Fatalf("diagnostics = %+v", got)
	}
}
