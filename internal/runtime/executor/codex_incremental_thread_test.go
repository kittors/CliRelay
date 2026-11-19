package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// incThreads is one Codex Desktop session: its root thread, whose id is the
// session id, and three sub-agents it spawned. Every thread sends the session's
// id (and over HTTP the session's prompt_cache_key); only the thread id tells
// them apart.
var incThreads = []string{
	"019a8c2e-5b7a-7c3e-9f00-00000000000a",
	"019a8c2e-6d10-7a41-8e00-00000000000b",
	"019a8c2e-7e21-7b52-8d00-00000000000c",
	"019a8c2e-8f32-7c63-8c00-00000000000d",
}

// incThreadCtx is the context of a request from one thread of the session, with
// the headers Codex sends over HTTP: a sub-agent also names its parent and what
// spawned it.
func incThreadCtx(thread int) context.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	session := incThreads[0]
	ginCtx.Request.Header.Set("Session_id", session)
	ginCtx.Request.Header.Set("Thread-Id", incThreads[thread])
	if thread > 0 {
		ginCtx.Request.Header.Set("X-Codex-Parent-Thread-Id", session)
		ginCtx.Request.Header.Set("X-Openai-Subagent", "collab_spawn")
	}
	return context.WithValue(incCtx(), util.ContextKeyGin, ginCtx)
}

func incThreadUser(thread, round int) string {
	return fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"thread %d, round %d"}]}`, thread, round)
}

// runInterleavedThreads has the threads take turns, the way a parent and its
// sub-agents run side by side: parent, each sub-agent, parent again, and so on.
func runInterleavedThreads(rounds int, turn func(thread, round int)) {
	for round := 0; round < rounds; round++ {
		for thread := range incThreads {
			turn(thread, round)
		}
	}
}

// checkEveryThreadContinued asserts that each thread's first turn went out in
// full and every later one as an increment on that thread's own connection. The
// fake backend keeps only the last response of a connection, so an increment
// sent on another thread's connection would be refused and sent again.
func checkEveryThreadContinued(t *testing.T, upstream *incUpstream, pool *codexIncrementalPool, rounds int) {
	t.Helper()
	threads := int64(len(incThreads))
	turns := threads * int64(rounds)
	got := pool.stats.byMode["on"]
	if got == nil || got.turns != turns || got.hits != turns-threads || got.reasons["no_previous_turn"] != threads {
		t.Fatalf("stats = %+v, want each thread's first turn in full and the other %d as increments", got, turns-threads)
	}
	if conns, ws, httpRequests := upstream.counts(); int64(conns) != threads || int64(ws) != turns || httpRequests != 0 {
		t.Fatalf("conns=%d websocket messages=%d http=%d, want one connection per thread carrying all of its turns", conns, ws, httpRequests)
	}
	continued := map[int]bool{}
	for _, msg := range upstream.websocketMessages() {
		if gjson.Get(msg.raw, "previous_response_id").Exists() != continued[msg.conn] {
			t.Fatalf("message on conn %d = %s, want the first in full and the rest as increments", msg.conn, msg.raw)
		}
		continued[msg.conn] = true
	}
}

// A Codex Desktop session runs its root thread and the sub-agents it spawned
// side by side. They share the session's identity and the opening of their
// history, so they used to share one conversation key, and four threads on
// three chains kept taking each other's chain: in production 37% of sub-agent
// turns missed with input_prefix_mismatch. Each thread is now its own line, on
// every entry the session can reach the relay through.
func TestCodexIncrementalKeepsEachThreadOnItsOwnLine(t *testing.T) {
	const rounds = 3
	replies := [][]string{{incReplyText1}, {incReplyText2}, {incReplyText3}}

	t.Run("responses over HTTP", func(t *testing.T) {
		upstream := newIncUpstream(t, replies...)
		exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
		histories := make([][]string, len(incThreads))
		runInterleavedThreads(rounds, func(thread, round int) {
			histories[thread] = append(histories[thread], incThreadUser(thread, round))
			if round == 0 {
				histories[thread] = append([]string{incDeveloper}, histories[thread]...)
			}
			// Codex sends the session id as prompt_cache_key from every thread.
			body := `{"model":"gpt-5.4","stream":true,"prompt_cache_key":"` + incThreads[0] + `","input":[` + strings.Join(histories[thread], ",") + `]}`
			final := runResponsesTurn(t, exec, incThreadCtx(thread), upstream.auth(), cliproxyexecutor.Options{}, body)
			for _, item := range final.Get("response.output").Array() {
				histories[thread] = append(histories[thread], item.Raw)
			}
		})
		checkEveryThreadContinued(t, upstream, pool, rounds)
	})

	t.Run("chat through a gateway that forwards the Codex headers", func(t *testing.T) {
		upstream := newIncUpstream(t, replies...)
		exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
		// What the handler derives from the Session_id header every thread sends.
		opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.SessionStickyMetadataKey: "header:session_id:" + incThreads[0]}}
		histories := make([][]string, len(incThreads))
		runInterleavedThreads(rounds, func(thread, round int) {
			if round == 0 {
				histories[thread] = []string{`{"role":"system","content":"be brief"}`}
			}
			histories[thread] = append(histories[thread], fmt.Sprintf(`{"role":"user","content":"thread %d, round %d"}`, thread, round))
			reply := runChatTurnIn(t, exec, incThreadCtx(thread), upstream.auth(), opts, histories[thread])
			histories[thread] = append(histories[thread], reply.message())
		})
		checkEveryThreadContinued(t, upstream, pool, rounds)
	})

	t.Run("client websocket naming the thread in client_metadata", func(t *testing.T) {
		upstream := newIncUpstream(t, replies...)
		exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
		// Over the client websocket per-request headers travel in
		// client_metadata; one socket carrying several threads still keeps them
		// apart.
		opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "client-ws-shared"}}
		histories := make([][]string, len(incThreads))
		runInterleavedThreads(rounds, func(thread, round int) {
			histories[thread] = append(histories[thread], incThreadUser(thread, round))
			if round == 0 {
				histories[thread] = append([]string{incDeveloper}, histories[thread]...)
			}
			meta, _ := sjson.Set(`{}`, "session_id", incThreads[0])
			meta, _ = sjson.Set(meta, "thread_id", incThreads[thread])
			meta, _ = sjson.Set(meta, "turn_id", fmt.Sprintf("turn-%d-%d", thread, round))
			body, _ := sjson.Set(responsesBody(histories[thread]...), "client_metadata.x-codex-turn-metadata", meta)
			final := runResponsesTurn(t, exec, incCtx(), upstream.auth(), opts, body)
			for _, item := range final.Get("response.output").Array() {
				histories[thread] = append(histories[thread], item.Raw)
			}
		})
		checkEveryThreadContinued(t, upstream, pool, rounds)
	})
}

// The thread refines the conversation key; who is calling still separates keys,
// and a request that names no thread keeps the key it had.
func TestCodexIncrementalConversationKeyNamesTheThread(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responses := sdktranslator.FromString("openai-response")
	withHeaders := func(headers map[string]string) context.Context {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for name, value := range headers {
			ginCtx.Request.Header.Set(name, value)
		}
		return context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	}
	bodyWith := func(clientMetadata string) []byte {
		body := `{"model":"gpt-5.4","prompt_cache_key":"pck-1","input":[` + incUser1 + `]}`
		if clientMetadata != "" {
			body, _ = sjson.SetRaw(body, "client_metadata", clientMetadata)
		}
		return []byte(body)
	}
	const tenant = "00000000-0000-0000-0000-0000000000aa"
	// clientWebsocket is the execution session of a client websocket, "" for
	// HTTP.
	key := func(ctx context.Context, body []byte, apiKey, tenant, clientWebsocket string) string {
		t.Helper()
		metadata := map[string]any{cliproxyexecutor.TenantMetadataKey: tenant}
		if clientWebsocket != "" {
			metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = clientWebsocket
		}
		execCtx := &ExecutionContext{
			Context:      ctx,
			Options:      cliproxyexecutor.Options{Metadata: metadata},
			Request:      cliproxyexecutor.Request{Payload: body},
			SourceFormat: responses,
		}
		got := codexIncrementalConversationKey(execCtx, mustParseIncremental(t, body), apiKey)
		if got == "" {
			t.Fatalf("no key for %s", body)
		}
		return got
	}
	plain := bodyWith("")
	threadHeader := func(thread string) context.Context { return withHeaders(map[string]string{"Thread-Id": thread}) }

	none := key(context.Background(), plain, "sk-a", tenant, "")
	if again := key(withHeaders(map[string]string{"Session_id": "s-1"}), plain, "sk-a", tenant, ""); again != none {
		t.Fatal("a request that names no thread changed key")
	}
	threadA := key(threadHeader("thread-a"), plain, "sk-a", tenant, "")
	threadB := key(threadHeader("thread-b"), plain, "sk-a", tenant, "")
	if threadA == none || threadA == threadB {
		t.Fatalf("keys none=%s a=%s b=%s, want each thread its own", none, threadA, threadB)
	}
	if again := key(threadHeader("thread-a"), plain, "sk-a", tenant, ""); again != threadA {
		t.Fatal("the same thread changed key between requests")
	}
	if got := key(withHeaders(map[string]string{"X-Codex-Turn-Metadata": `{"session_id":"s-1","thread_id":"thread-a","turn_id":"t-9"}`}), plain, "sk-a", tenant, ""); got != threadA {
		t.Errorf("thread named by the turn metadata header: key = %s, want thread-a's %s", got, threadA)
	}
	if got := key(withHeaders(map[string]string{"X-Codex-Turn-Metadata": `not json`}), plain, "sk-a", tenant, ""); got != none {
		t.Error("unreadable turn metadata named a thread")
	}

	// On the client websocket the handshake may name no thread; each request
	// names it in client_metadata, the way Codex sends its per-request headers
	// there.
	socketA := key(threadHeader("thread-a"), plain, "sk-a", tenant, "ws-1")
	for name, ctxBody := range map[string]struct {
		ctx  context.Context
		body []byte
	}{
		"client_metadata.thread_id":          {context.Background(), bodyWith(`{"thread_id":"thread-a","turn_id":"t-9"}`)},
		"client_metadata turn metadata":      {context.Background(), bodyWith(`{"x-codex-turn-metadata":"{\"thread_id\":\"thread-a\",\"turn_id\":\"t-9\"}"}`)},
		"handshake header over the body":     {threadHeader("thread-a"), bodyWith(`{"thread_id":"thread-b"}`)},
		"turn metadata header over the body": {withHeaders(map[string]string{"X-Codex-Turn-Metadata": `{"thread_id":"thread-a"}`}), bodyWith(`{"thread_id":"thread-b"}`)},
	} {
		if got := key(ctxBody.ctx, ctxBody.body, "sk-a", tenant, "ws-1"); got != socketA {
			t.Errorf("%s: key = %s, want thread-a's %s", name, got, socketA)
		}
	}
	if key(context.Background(), bodyWith(`{"thread_id":"thread-b"}`), "sk-a", tenant, "ws-1") == socketA || key(context.Background(), plain, "sk-a", tenant, "ws-1") == socketA {
		t.Error("another thread, or none, on the socket got thread-a's key")
	}
	// The body going upstream has had its client_metadata rewritten by
	// fingerprint convergence, which in its session and full modes gives every
	// thread one id. The key follows what the client sent.
	converged := &ExecutionContext{
		Context:      context.Background(),
		Options:      cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenant, cliproxyexecutor.ExecutionSessionMetadataKey: "ws-1"}},
		Request:      cliproxyexecutor.Request{Payload: bodyWith(`{"thread_id":"thread-a"}`)},
		SourceFormat: responses,
	}
	if got := codexIncrementalConversationKey(converged, mustParseIncremental(t, bodyWith(`{"thread_id":"converged"}`)), "sk-a"); got != socketA {
		t.Errorf("key = %s, want thread-a's %s whatever the upstream body says", got, socketA)
	}

	// Two callers sending the same thread never share a line.
	if key(threadHeader("thread-a"), plain, "sk-b", tenant, "") == threadA {
		t.Error("another API key got thread-a's key")
	}
	if key(threadHeader("thread-a"), plain, "sk-a", "00000000-0000-0000-0000-0000000000bb", "") == threadA {
		t.Error("another tenant got thread-a's key")
	}
}
