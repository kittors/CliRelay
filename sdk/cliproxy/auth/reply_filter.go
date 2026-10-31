package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
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
func filterReplyStream(ctx context.Context, filter StreamReplyFilter, result *cliproxyexecutor.StreamResult) *cliproxyexecutor.StreamResult {
	if filter == nil || result == nil {
		return result
	}
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
			if since, held := filter.HeldSince(); held && forward {
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
					sendAll(filter.Flush())
					return
				}
				if !forward {
					continue
				}
				if chunk.Err != nil {
					sendAll(filter.Flush())
					send(chunk)
					continue
				}
				sendAll(filter.Push(chunk.Payload))
			case <-expired:
				sendAll(filter.Flush())
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}
