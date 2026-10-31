package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// publishingStreamExecutor publishes a record per stream as it ends, through
// routetrace.DeferPublish as the usage reporters do, and keeps the trace each
// record went out with.
type publishingStreamExecutor struct {
	*scriptedStreamExecutor
	rec *routetrace.Recorder

	mu   sync.Mutex
	rows []*routetrace.Trace
}

func (e *publishingStreamExecutor) publish(ctx context.Context) {
	publish := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.rows = append(e.rows, e.rec.Snapshot())
	}
	if !routetrace.DeferPublish(ctx, publish) {
		publish()
	}
}

func (e *publishingStreamExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	result, err := e.scriptedStreamExecutor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		e.publish(ctx)
		return nil, err
	}
	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(out)
		for chunk := range result.Chunks {
			out <- chunk
		}
		e.publish(ctx)
	}()
	return &coreexecutor.StreamResult{Chunks: out}, nil
}

func (e *publishingStreamExecutor) Rows() []*routetrace.Trace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*routetrace.Trace(nil), e.rows...)
}

// The handler's bootstrap retries continue the request's route trace, and the
// record of each stream it abandoned goes out once it moved on, so that row
// names the account it moved to.
func TestStreamFailoverContinuesTheRouteTrace(t *testing.T) {
	rec := routetrace.New()
	executor := &publishingStreamExecutor{rec: rec, scriptedStreamExecutor: &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
		failBeforeFirstByte(http.StatusBadGateway),
		failBeforeFirstByte(http.StatusServiceUnavailable),
		succeed("ok"),
	}}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-trace-a", "sf-trace-b", "sf-trace-c")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	ctx := routetrace.WithRecorder(context.Background(), rec)
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	if out := drainStream(t, dataChan, errChan); out.err != nil || out.payload != "ok" {
		t.Fatalf("outcome = %+v", out)
	}

	ids := executor.AuthIDs()
	steps := rec.Snapshot().Steps
	if len(ids) != 3 || len(steps) != 3 {
		t.Fatalf("attempts = %v, steps = %+v", ids, steps)
	}
	for i, wantStatus := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, 0} {
		step := steps[i]
		if step.Attempt != i+1 || step.AuthID != ids[i] || step.Status != wantStatus || (wantStatus != 0) != step.BeforeFirstByte {
			t.Fatalf("step %d = %+v, want attempt %d on %s with status %d", i, step, i+1, ids[i], wantStatus)
		}
	}
	rows := executor.Rows()
	if len(rows) != 3 {
		t.Fatalf("published %d records, want 3", len(rows))
	}
	for i := 0; i < 2; i++ {
		if row := rows[i].Steps; len(row) != i+2 || row[i+1].AuthID != ids[i+1] {
			t.Fatalf("row %d = %+v, want it to name the next account %s", i, row, ids[i+1])
		}
	}
	if last := rows[2].Steps; len(last) != 3 || last[2].Outcome != routetrace.OutcomeSuccess {
		t.Fatalf("last row = %+v", last)
	}
}
