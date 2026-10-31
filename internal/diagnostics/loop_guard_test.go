package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func newDiagnosticContext(t *testing.T) (*gin.Context, *RequestDiagnostic) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	return c, EnsureGin(c, "req-loop")
}

func sampleReplyLoop() LoopGuardSnapshot {
	return LoopGuardSnapshot{
		Kind: LoopGuardReplyLoop, Source: "api_key",
		Window: 256, MaxDistinct: 24, Distinct: 3, Units: []string{"Okay.", "Hmm."},
		Reasoning: true, UnitsRead: 256, CutAtChunk: 412, CutAtBytes: 51234,
	}
}

func TestSetLoopGuardIsFoundFromTheRequestContext(t *testing.T) {
	c, d := newDiagnosticContext(t)
	SetLoopGuard(c.Request.Context(), sampleReplyLoop())

	snapshot := d.Snapshot()
	if snapshot.LoopGuard == nil || snapshot.LoopGuard.Kind != LoopGuardReplyLoop || snapshot.LoopGuard.CutAtChunk != 412 {
		t.Fatalf("snapshot = %+v", snapshot.LoopGuard)
	}
	if snapshot.IsZero() {
		t.Fatal("a snapshot holding only the loop guard section is empty")
	}
}

// The handlers hand over their own context, which carries the gin context
// rather than the request's diagnostic.
func TestSetLoopGuardIsFoundFromTheHandlerContext(t *testing.T) {
	c, d := newDiagnosticContext(t)
	handlerCtx := context.WithValue(context.Background(), util.ContextKeyGin, c)
	SetLoopGuard(handlerCtx, LoopGuardSnapshot{Kind: LoopGuardToolLoop, Repeats: 8, Turns: 9,
		Calls: []LoopGuardCall{{Name: "Read", Arguments: `{"file_path":"/a"}`}}})

	got := d.Snapshot().LoopGuard
	if got == nil || got.Kind != LoopGuardToolLoop || got.Turns != 9 || len(got.Calls) != 1 || got.Calls[0].Name != "Read" {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestSetLoopGuardWithoutADiagnosticDoesNothing(t *testing.T) {
	SetLoopGuard(context.Background(), sampleReplyLoop()) // must not panic
	SetLoopGuard(nil, sampleReplyLoop())                  //nolint:staticcheck // a nil context is what an absent request looks like
}

func TestLoopGuardSnapshotsAreCopies(t *testing.T) {
	c, d := newDiagnosticContext(t)
	in := sampleReplyLoop()
	SetLoopGuard(c.Request.Context(), in)

	in.Units[0] = "changed by the caller"
	first := d.Snapshot()
	if first.LoopGuard.Units[0] != "Okay." {
		t.Fatal("the stored snapshot shares its units with the caller")
	}
	first.LoopGuard.Units[0] = "changed by the reader"
	first.LoopGuard.Kind = "x"
	if second := d.Snapshot(); second.LoopGuard.Units[0] != "Okay." || second.LoopGuard.Kind != LoopGuardReplyLoop {
		t.Fatal("a reader can edit the stored snapshot")
	}
}

// The panel and the request log read these names.
func TestLoopGuardSnapshotJSON(t *testing.T) {
	c, d := newDiagnosticContext(t)
	SetLoopGuard(c.Request.Context(), sampleReplyLoop())
	data, err := json.Marshal(d.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var section map[string]any
	if err := json.Unmarshal(root["loop_guard"], &section); err != nil {
		t.Fatalf("no loop_guard section in %s", data)
	}
	for _, field := range []string{"kind", "source", "window", "max_distinct", "distinct", "units", "reasoning", "units_read", "cut_at_chunk", "cut_at_bytes"} {
		if _, ok := section[field]; !ok {
			t.Fatalf("loop_guard has no %q: %v", field, section)
		}
	}
	// A request nothing happened to has no section at all.
	_, d2 := newDiagnosticContext(t)
	data, _ = json.Marshal(d2.Snapshot())
	if strings.Contains(string(data), "loop_guard") {
		t.Fatalf("an untouched request reports a loop_guard section: %s", data)
	}
}
