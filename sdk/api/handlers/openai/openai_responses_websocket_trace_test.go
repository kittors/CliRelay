package openai

import (
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	"github.com/tidwall/gjson"
)

func waitForTraceStage(t *testing.T, rec *routetrace.Recorder, stage string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rec.Live().Stage != stage {
		if time.Now().After(deadline) {
			t.Fatalf("route trace stage = %q, want %q", rec.Live().Stage, stage)
		}
		time.Sleep(time.Millisecond)
	}
}

// One WebSocket connection runs its requests one after another on the
// connection's route trace: each request starts the trace over, and between
// requests the connection is idle, which keeps it off the in-flight list.
func TestResponsesWebsocketStartsTheRouteTraceOverForEachRequest(t *testing.T) {
	exec := newWSTurnExecutor(t)
	recorders := make(chan *routetrace.Recorder, 1)
	s := newWSTurnServerWith(t, exec, func(c *gin.Context) {
		rec := routetrace.New()
		c.Request = c.Request.WithContext(routetrace.WithRecorder(c.Request.Context(), rec))
		recorders <- rec
	})
	conn := s.dial()
	defer s.hangUp(conn)
	rec := <-recorders
	waitForTraceStage(t, rec, routetrace.StageIdle)

	for turn := 1; turn <= 2; turn++ {
		wsTurnSend(t, conn, wsTurnCreate)
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("turn %d reply = %s", turn, reply)
		}
		waitForTraceStage(t, rec, routetrace.StageIdle)
		trace := rec.Snapshot()
		if trace == nil || len(trace.Steps) != 1 {
			t.Fatalf("turn %d trace = %+v, want this request's one attempt", turn, trace)
		}
		if step := trace.Steps[0]; step.Attempt != 1 || step.Outcome != routetrace.OutcomeSuccess || !strings.EqualFold(step.Provider, exec.provider) {
			t.Fatalf("turn %d attempt = %+v", turn, step)
		}
	}
}
