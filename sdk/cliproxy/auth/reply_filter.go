package auth

import (
	"context"
	"runtime/debug"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// replyHoldLimit bounds how long a stream filter may keep part of a reply back
// while it waits for the text that follows. A filter holds text back so that a
// word split between two stream events is still seen whole; if nothing follows
// for this long, the held text is released as it is rather than stalling the
// client behind a model that has stopped talking.
const replyHoldLimit = 2 * time.Second

// ReplyFilter rewrites what the upstream sends back to the client. The host
// returns one in RequestGuardResult.Reply for a request whose reply it wants
// to filter; the manager applies it to whatever the execution returns.
//
// What the filter may change is the host's business: it is handed the
// executor's output in the client's protocol and returns the same protocol.
//
// A filter must not panic, but a bug in one must not cost the client its reply
// (and, for a stream, the process: the stream filter runs in a goroutine of
// its own). The manager therefore recovers from a panic, logs it, and sends the
// rest of that reply on unfiltered. A filter or stream filter that also has an
// Abandoned(recovered any) method is told when that happens, so the host can
// note it where it keeps its own records.
type ReplyFilter interface {
	// FilterResponse rewrites a complete (non-streaming) reply. The returned
	// response replaces the original; headers that no longer describe the body
	// (its length, and the encoding if the filter decoded it) must be dropped.
	FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response
	// NewStream starts the filter for one streaming reply. Streams do not share
	// state, so a request that is retried gets a fresh one.
	NewStream() StreamReplyFilter
}

// StreamReplyFilter filters one stream, chunk by chunk, in the order the
// chunks arrive. It is used from a single goroutine.
type StreamReplyFilter interface {
	// Push takes the next chunk and returns the chunks to send on, possibly
	// none (the filter is holding the chunk back) and possibly several.
	Push(chunk []byte) [][]byte
	// Flush returns everything still held back. The manager calls it when the
	// stream ends, before it delivers a stream error, and when HeldSince says
	// the hold has outlasted replyHoldLimit.
	Flush() [][]byte
	// HeldSince reports when the oldest chunk still held back was taken, and
	// false when nothing is held.
	HeldSince() (time.Time, bool)
}

// filterReplyStream puts the filter between a stream and its reader. Chunks the
// filter returns keep the order they were pushed in; errors and the end of the
// stream release whatever the filter holds first, so no text is lost behind
// them.
func filterReplyStream(ctx context.Context, stream StreamReplyFilter, result *cliproxyexecutor.StreamResult) *cliproxyexecutor.StreamResult {
	if stream == nil || result == nil {
		return result
	}
	filter := &guardedStream{filter: stream}
	out := make(chan cliproxyexecutor.StreamChunk)
	in := result.Chunks
	go func() {
		defer close(out)
		// Once the reader is gone the input is still drained, so the producer
		// behind it is never left blocked on a send nobody receives.
		forward := true
		send := func(chunk cliproxyexecutor.StreamChunk) {
			if !forward {
				return
			}
			if ctx == nil {
				out <- chunk
				return
			}
			select {
			case <-ctx.Done():
				forward = false
			case out <- chunk:
			}
		}
		sendAll := func(payloads [][]byte) {
			for _, payload := range payloads {
				if len(payload) > 0 {
					send(cliproxyexecutor.StreamChunk{Payload: payload})
				}
			}
		}
		var timer *time.Timer
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			// While text is held back, the wait for the next chunk has a deadline.
			var expired <-chan time.Time
			if since, held := filter.heldSince(); held && forward {
				wait := max(time.Until(since.Add(replyHoldLimit)), 0)
				if timer == nil {
					timer = time.NewTimer(wait)
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(wait)
				}
				expired = timer.C
			}
			select {
			case chunk, ok := <-in:
				if !ok {
					sendAll(filter.flush())
					return
				}
				if !forward {
					continue
				}
				if chunk.Err != nil {
					sendAll(filter.flush())
					send(chunk)
					continue
				}
				sendAll(filter.push(chunk.Payload))
			case <-expired:
				sendAll(filter.flush())
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

// abandonNotifier is implemented by a reply filter that wants to be told the
// manager stopped using it because it panicked.
type abandonNotifier interface{ Abandoned(recovered any) }

// replyFilterFailed records a filter panic: loudly in the log, and with the
// filter if it asked to hear of it.
func replyFilterFailed(filter any, recovered any) {
	log.WithField("panic", recovered).Errorf("reply filter panicked; the rest of this reply is sent unfiltered\n%s", debug.Stack())
	if notifier, ok := filter.(abandonNotifier); ok {
		func() {
			defer func() { _ = recover() }()
			notifier.Abandoned(recovered)
		}()
	}
}

// filterReplyResponse applies the filter to a complete reply. A filter that
// panics costs the client nothing: the reply goes on as it came.
func filterReplyResponse(filter ReplyFilter, resp cliproxyexecutor.Response) (out cliproxyexecutor.Response) {
	defer func() {
		if recovered := recover(); recovered != nil {
			replyFilterFailed(filter, recovered)
			out = resp
		}
	}()
	return filter.FilterResponse(resp)
}

// newReplyStreamFilter starts the filter for a stream, or returns nil if it
// cannot.
func newReplyStreamFilter(filter ReplyFilter) (stream StreamReplyFilter) {
	defer func() {
		if recovered := recover(); recovered != nil {
			replyFilterFailed(filter, recovered)
			stream = nil
		}
	}()
	return filter.NewStream()
}

// guardedStream keeps a stream filter's panics from reaching the goroutine
// that drives it, where nothing would catch them and the process would end.
// After a panic the filter's state cannot be trusted: whatever it still holds
// is released if it will give it up, and every later chunk passes unchanged.
type guardedStream struct {
	filter StreamReplyFilter
	failed bool
}

func (g *guardedStream) fail(recovered any) {
	g.failed = true
	replyFilterFailed(g.filter, recovered)
}

func (g *guardedStream) heldSince() (since time.Time, held bool) {
	if g.failed {
		return time.Time{}, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			g.fail(recovered)
			since, held = time.Time{}, false
		}
	}()
	return g.filter.HeldSince()
}

func (g *guardedStream) push(chunk []byte) (out [][]byte) {
	if g.failed {
		return [][]byte{chunk}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			g.fail(recovered)
			// What was held back precedes the chunk that broke the filter.
			out = append(g.release(), chunk)
		}
	}()
	return g.filter.Push(chunk)
}

func (g *guardedStream) flush() (out [][]byte) {
	if g.failed {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			g.fail(recovered)
			out = nil
		}
	}()
	return g.filter.Flush()
}

// release asks a failed filter for what it holds, once, giving up silently if
// it cannot.
func (g *guardedStream) release() (out [][]byte) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return g.filter.Flush()
}
