package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// sseKeepAliveComment is the heartbeat ForwardStream sends by default; waits
// before the stream has started send the same one.
const sseKeepAliveComment = ": keep-alive\n\n"

// streamStartWaits keeps a streaming client informed while the scheduler waits
// for a cooling account before the stream has started.
//
// The trade-off: as long as nothing has been written, a request that finally
// fails can still answer with its real status and Retry-After header; once a
// keep-alive has gone out, the response is committed to 200 and a failure can
// only be reported as the protocol's in-stream error event. So the wait stays
// silent for up to one keep-alive interval, which is how long a slow upstream
// has always gone without a heartbeat, and only a longer wait commits the
// response and then sends the same heartbeat ForwardStream sends, at the same
// interval. Short waits keep the exact error; long ones keep the connection.
//
// Only SSE responses get heartbeats: not the Gemini non-SSE stream (alt set),
// not a downstream websocket, and nothing when keep-alives are switched off.
// Once StartStreamWithAuthManager hands the stream to the protocol handler,
// ForwardStream owns the writer and any later wait (a retry on another
// account) is silent here because ForwardStream's own heartbeat covers it.
type streamStartWaits struct {
	h        *BaseAPIHandler
	c        *gin.Context
	flusher  http.Flusher
	interval time.Duration

	mu         sync.Mutex
	handedOver bool
	committed  bool
	silent     time.Duration
	lastBeat   time.Time
}

// startStreamWaits prepares ctx for a streaming request: every scheduling pass
// shares one busy-wait budget, and waits go through the returned tracker.
func (h *BaseAPIHandler) startStreamWaits(ctx context.Context, alt string) (context.Context, *streamStartWaits) {
	w := &streamStartWaits{h: h}
	if c := ginContextFrom(ctx); c != nil && alt == "" && !coreexecutor.DownstreamWebsocket(ctx) {
		if flusher, ok := c.Writer.(http.Flusher); ok {
			if interval := StreamingKeepAliveInterval(h.Cfg); interval > 0 {
				w.c, w.flusher, w.interval = c, flusher, interval
			}
		}
	}
	ctx = coreauth.WithRetryWaitBudget(ctx)
	return coreauth.WithRetryWaiter(ctx, w.wait), w
}

// handOver ends the tracker's part: from now on the protocol handler writes.
// It reports whether the response was already committed by a heartbeat.
func (w *streamStartWaits) handOver() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handedOver = true
	return w.committed
}

func (w *streamStartWaits) wait(ctx context.Context, wait time.Duration) error {
	w.note(ctx, wait)
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		step := remaining
		if due, ok := w.untilHeartbeat(); ok && due < step {
			step = due
		}
		started := time.Now()
		if err := sleepContext(ctx, step); err != nil {
			return err
		}
		w.afterSleep(time.Since(started))
	}
}

func (w *streamStartWaits) untilHeartbeat() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c == nil || w.handedOver {
		return 0, false
	}
	due := w.interval - w.silent
	if w.committed {
		due = w.interval - time.Since(w.lastBeat)
	}
	return max(due, 0), true
}

func (w *streamStartWaits) afterSleep(slept time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c == nil || w.handedOver {
		return
	}
	if !w.committed {
		w.silent += slept
		if w.silent < w.interval {
			return
		}
		PrepareStreamingResponse(w.c)
		w.committed = true
	} else if time.Since(w.lastBeat) < w.interval {
		return
	}
	_, _ = w.c.Writer.Write([]byte(sseKeepAliveComment))
	w.flusher.Flush()
	w.lastBeat = time.Now()
}

// note records the wait in the request log, next to the attempts around it.
func (w *streamStartWaits) note(ctx context.Context, wait time.Duration) {
	if w.h == nil || w.h.Cfg == nil || !w.h.Cfg.RequestLog {
		return
	}
	appendAPIResponse(ginContextFrom(ctx), []byte(fmt.Sprintf(
		"[retry wait] every account that can take this request is cooling down; waiting %s for the soonest to recover",
		wait.Round(100*time.Millisecond),
	)))
}

// failedAfterCommit delivers a start failure as an in-stream error: a
// heartbeat already committed the response, so the protocol handler must
// write its terminal error event instead of a JSON error with a status code.
func failedAfterCommit(msg *StreamErrorMessage) (<-chan []byte, http.Header, <-chan *StreamErrorMessage, *StreamErrorMessage) {
	dataChan := make(chan []byte)
	close(dataChan)
	errChan := make(chan *StreamErrorMessage, 1)
	errChan <- msg
	close(errChan)
	return dataChan, nil, errChan, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
