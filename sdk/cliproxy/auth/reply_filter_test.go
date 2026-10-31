package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// delayFilter holds each chunk back until the next arrives, like a filter that
// is waiting to see whether a word continues. Chunks starting with "!" are
// let through at once and do not release the held one.
type delayFilter struct {
	mu    sync.Mutex
	held  []byte
	since time.Time
}

func (f *delayFilter) Push(chunk []byte) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(string(chunk), "!") {
		return [][]byte{chunk}
	}
	var out [][]byte
	if f.held != nil {
		out = append(out, f.held)
	}
	f.held, f.since = chunk, time.Now()
	return out
}

func (f *delayFilter) Flush() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		return nil
	}
	out := [][]byte{f.held}
	f.held = nil
	return out
}

func (f *delayFilter) HeldSince() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since, f.held != nil
}

func feed(chunks ...cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	in := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		in <- chunk
	}
	close(in)
	return &cliproxyexecutor.StreamResult{Chunks: in}
}

func text(payload string) cliproxyexecutor.StreamChunk {
	return cliproxyexecutor.StreamChunk{Payload: []byte(payload)}
}

func drain(result *cliproxyexecutor.StreamResult) (payloads []string, errs []error) {
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			errs = append(errs, chunk.Err)
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	return payloads, errs
}

func TestFilterReplyStreamKeepsOrderAndReleasesTheLastChunkAtTheEnd(t *testing.T) {
	result := filterReplyStream(context.Background(), &delayFilter{}, feed(text("a"), text("!ping"), text("b"), text("c")))
	got, errs := drain(result)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// "!ping" overtakes the held "a": the filter said it may.
	if want := "!ping,a,b,c"; strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestFilterReplyStreamReleasesHeldTextBeforeAnError(t *testing.T) {
	boom := errors.New("boom")
	result := filterReplyStream(context.Background(), &delayFilter{}, feed(text("a"), cliproxyexecutor.StreamChunk{Err: boom}))
	var order []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			order = append(order, "error")
		} else {
			order = append(order, string(chunk.Payload))
		}
	}
	if strings.Join(order, ",") != "a,error" {
		t.Fatalf("order = %v, want the held text before the error", order)
	}
}

func TestFilterReplyStreamKeepsTheUpstreamHeaders(t *testing.T) {
	in := feed(text("a"))
	in.Headers = map[string][]string{"X-Upstream": {"1"}}
	out := filterReplyStream(context.Background(), &delayFilter{}, in)
	if out.Headers.Get("X-Upstream") != "1" {
		t.Fatalf("headers = %v", out.Headers)
	}
	drain(out)
}

func TestFilterReplyStreamWithoutAFilterIsTheStream(t *testing.T) {
	in := feed(text("a"))
	if filterReplyStream(context.Background(), nil, in) != in {
		t.Fatal("a stream with no filter must come back unchanged")
	}
}

// Text held back for a word that never continues must not wait for the model
// forever: after the limit it is released although nothing else arrived.
func TestFilterReplyStreamReleasesHeldTextWhenTheModelGoesQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := make(chan cliproxyexecutor.StreamChunk)
		result := filterReplyStream(context.Background(), &delayFilter{}, &cliproxyexecutor.StreamResult{Chunks: in})

		in <- text("held")
		start := time.Now()
		chunk := <-result.Chunks
		if string(chunk.Payload) != "held" {
			t.Fatalf("got %q", chunk.Payload)
		}
		if waited := time.Since(start); waited != replyHoldLimit {
			t.Fatalf("held text was released after %v, want exactly %v", waited, replyHoldLimit)
		}
		close(in)
		if _, ok := <-result.Chunks; ok {
			t.Fatal("stream did not end")
		}
	})
}

// A chatty upstream (keep-alives, other events) must not keep text held past
// the limit: the limit runs from when the text was taken, not from the last
// chunk.
func TestFilterReplyStreamHoldLimitIsNotResetByTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := make(chan cliproxyexecutor.StreamChunk)
		result := filterReplyStream(context.Background(), &delayFilter{}, &cliproxyexecutor.StreamResult{Chunks: in})
		start := time.Now()
		in <- text("held")
		go func() {
			for i := 0; i < 10; i++ {
				time.Sleep(500 * time.Millisecond)
				in <- text("!keepalive")
			}
			close(in)
		}()
		var releasedAt time.Duration
		for chunk := range result.Chunks {
			if string(chunk.Payload) == "held" {
				releasedAt = time.Since(start)
			}
		}
		if releasedAt != replyHoldLimit {
			t.Fatalf("held text released after %v, want %v", releasedAt, replyHoldLimit)
		}
	})
}

// A reader that has gone away must not leave the producer blocked: the input
// keeps being drained until it closes.
func TestFilterReplyStreamDrainsTheInputAfterTheReaderLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		in := make(chan cliproxyexecutor.StreamChunk)
		result := filterReplyStream(ctx, &delayFilter{}, &cliproxyexecutor.StreamResult{Chunks: in})
		in <- text("a")
		in <- text("b")
		<-result.Chunks // "a"
		cancel()
		sent := 0
		for i := 0; i < 5; i++ {
			in <- text("more")
			sent++
		}
		close(in)
		synctest.Wait()
		if sent != 5 {
			t.Fatalf("producer sent %d chunks, want it never blocked", sent)
		}
	})
}

// brokenFilter is a stream filter with a bug: it panics where told to.
type brokenFilter struct {
	delayFilter
	panicPush  string
	panicFlush bool
	panicHeld  bool
	abandoned  []any
}

func (f *brokenFilter) Push(chunk []byte) [][]byte {
	if string(chunk) == f.panicPush {
		panic("push bug")
	}
	return f.delayFilter.Push(chunk)
}

func (f *brokenFilter) Flush() [][]byte {
	if f.panicFlush {
		panic("flush bug")
	}
	return f.delayFilter.Flush()
}

func (f *brokenFilter) HeldSince() (time.Time, bool) {
	if f.panicHeld {
		panic("held bug")
	}
	return f.delayFilter.HeldSince()
}

func (f *brokenFilter) Abandoned(recovered any) { f.abandoned = append(f.abandoned, recovered) }

// A filter that panics in its own goroutine would end the process. Instead the
// reply goes on unfiltered: what the filter held comes first, then the chunk
// that broke it, then everything after as it arrives.
func TestFilterReplyStreamSurvivesAFilterThatPanics(t *testing.T) {
	filter := &brokenFilter{panicPush: "bad"}
	result := filterReplyStream(context.Background(), filter, feed(text("a"), text("b"), text("bad"), text("c"), text("d")))
	got, errs := drain(result)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// "a" was released when "b" arrived; "b" was held; the panic releases "b",
	// and "bad", "c", "d" pass as they are.
	if want := "a,b,bad,c,d"; strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	if len(filter.abandoned) != 1 || filter.abandoned[0] != "push bug" {
		t.Fatalf("the filter was told %v", filter.abandoned)
	}
}

func TestFilterReplyStreamSurvivesAFilterThatPanicsOnFlush(t *testing.T) {
	filter := &brokenFilter{panicFlush: true}
	result := filterReplyStream(context.Background(), filter, feed(text("a"), cliproxyexecutor.StreamChunk{Err: errors.New("boom")}))
	got, errs := drain(result)
	// Nothing held can be recovered from a filter that will not give it up, but
	// the error still reaches the client.
	if len(errs) != 1 || len(got) != 0 {
		t.Fatalf("got %v and %v", got, errs)
	}
	if len(filter.abandoned) != 1 {
		t.Fatalf("abandoned %v", filter.abandoned)
	}
	// A stream that merely ends must close too.
	filter = &brokenFilter{panicFlush: true}
	got, errs = drain(filterReplyStream(context.Background(), filter, feed(text("x"), text("y"))))
	if len(errs) != 0 || strings.Join(got, ",") != "x" {
		t.Fatalf("got %v and %v", got, errs)
	}
}

func TestFilterReplyStreamSurvivesAFilterThatPanicsOnHeldSince(t *testing.T) {
	filter := &brokenFilter{panicHeld: true}
	got, errs := drain(filterReplyStream(context.Background(), filter, feed(text("a"), text("b"))))
	if len(errs) != 0 || strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v and %v", got, errs)
	}
	if len(filter.abandoned) != 1 {
		t.Fatalf("abandoned %v", filter.abandoned)
	}
}

type panickyReply struct {
	panicResponse, panicStream bool
	abandoned                  []any
}

func (r *panickyReply) FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response {
	if r.panicResponse {
		panic("response bug")
	}
	return resp
}

func (r *panickyReply) NewStream() StreamReplyFilter {
	if r.panicStream {
		panic("stream bug")
	}
	return &delayFilter{}
}

func (r *panickyReply) Abandoned(recovered any) { r.abandoned = append(r.abandoned, recovered) }

func TestAFilterThatPanicsOnAWholeReplyLeavesItAsItWas(t *testing.T) {
	reply := &panickyReply{panicResponse: true}
	in := cliproxyexecutor.Response{Payload: []byte("body")}
	if got := filterReplyResponse(reply, in); string(got.Payload) != "body" {
		t.Fatalf("got %q", got.Payload)
	}
	if len(reply.abandoned) != 1 {
		t.Fatalf("abandoned %v", reply.abandoned)
	}
}

func TestAFilterThatCannotStartAStreamLeavesTheStreamAlone(t *testing.T) {
	reply := &panickyReply{panicStream: true}
	if stream := newReplyStreamFilter(reply); stream != nil {
		t.Fatalf("got %v", stream)
	}
	in := feed(text("a"))
	if filterReplyStream(context.Background(), newReplyStreamFilter(reply), in) != in {
		t.Fatal("a stream whose filter could not start must come back unchanged")
	}
	if len(reply.abandoned) != 2 {
		t.Fatalf("abandoned %v", reply.abandoned)
	}
}
