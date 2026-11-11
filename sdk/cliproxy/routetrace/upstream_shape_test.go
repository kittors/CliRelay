package routetrace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAttachUpstreamRequestShapeLandsOnTheAttemptsStep(t *testing.T) {
	r := New()
	first := r.BeginAttempt(info("a"))
	second := r.BeginAttempt(info("b"))

	if !AttachUpstreamRequestShape(first.Context(context.Background()), func() json.RawMessage {
		return json.RawMessage(`{"bytes":{"plain":10}}`)
	}) {
		t.Fatal("an attempt context must accept a shape")
	}
	if !AttachUpstreamRequestShape(second.Context(context.Background()), func() json.RawMessage {
		return json.RawMessage(`{"bytes":{"plain":20}}`)
	}) {
		t.Fatal("an attempt context must accept a shape")
	}

	trace := r.Snapshot()
	if trace == nil || len(trace.Steps) != 2 {
		t.Fatalf("trace = %#v, want two attempts", trace)
	}
	// Each attempt keeps its own: the same body can fail on one account and pass
	// on another, and the headers it went out with differ per account.
	if got := string(trace.Steps[0].UpstreamRequestShape); got != `{"bytes":{"plain":10}}` {
		t.Fatalf("first attempt shape = %q", got)
	}
	if got := string(trace.Steps[1].UpstreamRequestShape); got != `{"bytes":{"plain":20}}` {
		t.Fatalf("second attempt shape = %q", got)
	}

	// The shape travels as JSON, not as an escaped string, so the request log
	// detail reads as one document.
	raw, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Steps []struct {
			Shape struct {
				Bytes struct {
					Plain int `json:"plain"`
				} `json:"bytes"`
			} `json:"upstream_request_shape"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("trace JSON: %v\n%s", err, raw)
	}
	if decoded.Steps[0].Shape.Bytes.Plain != 10 || decoded.Steps[1].Shape.Bytes.Plain != 20 {
		t.Fatalf("decoded trace = %s", raw)
	}
}

func TestAttachUpstreamRequestShapeKeepsTheLastShapeOfAnAttempt(t *testing.T) {
	r := New()
	attempt := r.BeginAttempt(info("a"))
	ctx := attempt.Context(context.Background())
	for _, shape := range []string{`{"n":1}`, `{"n":2}`} {
		AttachUpstreamRequestShape(ctx, func() json.RawMessage { return json.RawMessage(shape) })
	}
	if got := string(r.Snapshot().Steps[0].UpstreamRequestShape); got != `{"n":2}` {
		t.Fatalf("shape = %q, want the last one", got)
	}
}

// Building a shape reads the whole request body, so it must not run when there is
// nowhere to put the result.
func TestAttachUpstreamRequestShapeBuildsNothingWithoutAnAttempt(t *testing.T) {
	var nilRecorder *Recorder
	contexts := map[string]context.Context{
		"no attempt":              context.Background(),
		"nil context":             nil,
		"attempt of nil recorder": nilRecorder.BeginAttempt(info("a")).Context(context.Background()),
		"recorder but no attempt": WithRecorder(context.Background(), New()),
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			built := false
			ok := AttachUpstreamRequestShape(ctx, func() json.RawMessage {
				built = true
				return json.RawMessage(`{}`)
			})
			if ok || built {
				t.Fatalf("attached = %v, built = %v; want neither", ok, built)
			}
		})
	}
	if AttachUpstreamRequestShape(New().BeginAttempt(info("a")).Context(context.Background()), nil) {
		t.Fatal("a nil builder must attach nothing")
	}
}

func TestAttachUpstreamRequestShapeDropsEmptyAndOversizedShapes(t *testing.T) {
	r := New()
	ctx := r.BeginAttempt(info("a")).Context(context.Background())

	oversized := json.RawMessage(`{"x":"` + strings.Repeat("a", MaxUpstreamRequestShapeBytes) + `"}`)
	for name, shape := range map[string]json.RawMessage{"empty": nil, "oversized": oversized} {
		if AttachUpstreamRequestShape(ctx, func() json.RawMessage { return shape }) {
			t.Fatalf("a %s shape must be refused", name)
		}
	}
	if got := r.Snapshot().Steps[0].UpstreamRequestShape; got != nil {
		t.Fatalf("shape = %q, want none", got)
	}
	raw, _ := json.Marshal(r.Snapshot())
	if strings.Contains(string(raw), "upstream_request_shape") {
		t.Fatalf("a step without a shape must not name the field: %s", raw)
	}
}

// build reads the request body and may take a while; the recorder is shared with
// the scheduler, so it must not be locked meanwhile.
func TestAttachUpstreamRequestShapeBuildsOutsideTheRecordersLock(t *testing.T) {
	r := New()
	ctx := r.BeginAttempt(info("a")).Context(context.Background())
	AttachUpstreamRequestShape(ctx, func() json.RawMessage {
		if r.Snapshot() == nil { // would deadlock if the lock were held
			t.Error("trace missing")
		}
		return json.RawMessage(`{}`)
	})
}
