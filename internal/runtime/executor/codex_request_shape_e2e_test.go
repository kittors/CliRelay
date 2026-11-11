package executor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// shapeUsageCapture collects the usage records the executors publish.
type shapeUsageCapture struct{ records chan coreusage.Record }

func (c *shapeUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	select {
	case c.records <- record:
	default:
	}
}

// The whole path of an unexplained 400, with only the upstream stubbed: the
// scheduler places the request on an account and opens an attempt, the Codex
// executor sends it, the stub refuses it with {"detail":"Bad Request"}, the
// scheduler moves on to the next account, and each failed attempt's usage record
// leaves with its own shape in detail_content.
func TestUnexplainedBadRequestShapeReachesTheRequestLogThroughTheScheduler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var bodies [][]byte
	var accounts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "zstd" {
			dec, err := zstd.NewReader(nil)
			if err != nil {
				t.Errorf("zstd reader: %v", err)
			} else if raw, err = dec.DecodeAll(raw, nil); err != nil {
				t.Errorf("request body is not zstd: %v", err)
			}
		}
		mu.Lock()
		bodies = append(bodies, raw)
		accounts = append(accounts, r.Header.Get("Chatgpt-Account-Id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Bad Request"}`))
	}))
	t.Cleanup(server.Close)

	plugin := &shapeUsageCapture{records: make(chan coreusage.Record, 32)}
	coreusage.RegisterPlugin(plugin)

	// Three ChatGPT OAuth accounts, as in production. They carry no base URL, so
	// the executor addresses chatgpt.com and the transport below redirects it.
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0)
	manager.RegisterExecutor(executor.NewCodexExecutor(&config.Config{}))
	ids := []string{"shape-e2e-a", "shape-e2e-b", "shape-e2e-c"}
	for _, id := range ids {
		auth := &coreauth.Auth{
			ID: id, Provider: "codex", Status: coreauth.StatusActive,
			Metadata: map[string]any{"access_token": "token-" + id, "account_id": "account-" + id, "email": id + "@example.test"},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.4", Object: "model", OwnedBy: "codex"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}

	// What the gateway's request middleware sets up around the handler.
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"compaction","window_number":1,"history_initialization":"cold_resume"}`)
	diagnostic := diagnostics.EnsureGin(ginCtx, "req-shape-e2e")
	endRequest := diagnostics.BeginInFlight(diagnostic)
	ctx, cancel := context.WithTimeout(context.WithValue(ginCtx.Request.Context(), util.ContextKeyGin, ginCtx), 20*time.Second)
	defer cancel()
	ctx = coreexecutor.WithRoundTripper(ctx, redirectTransport{to: server.URL})

	payload := `{"model":"gpt-5.4","store":false,"reasoning":{"effort":"low"},"input":[` +
		`{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"first turn"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAAABp` + strings.Repeat("Z", 160) + `"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
		`{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"a.txt\nb.txt"},` +
		`{"type":"message","id":"msg_2","role":"user","content":[{"type":"input_text","text":"next turn"}]}],` +
		`"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	source := sdktranslator.FromString("openai-response")
	_, err := manager.Execute(ctx, []string{"codex"},
		coreexecutor.Request{Model: "gpt-5.4", Payload: []byte(payload), Format: source},
		coreexecutor.Options{SourceFormat: source})
	if err == nil {
		t.Fatal("the stub refuses every request, so the call must fail")
	}
	// The request is over: whatever the scheduler still held goes out.
	endRequest()

	mu.Lock()
	sent := append([][]byte(nil), bodies...)
	sentTo := append([]string(nil), accounts...)
	mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("the stub never saw a request")
	}

	// Every attempt's record is published; the last one carries the whole trace.
	var detail string
	for got := 0; got < len(sent); {
		select {
		case record := <-plugin.records:
			if strings.Contains(record.DetailContent, "req-shape-e2e") {
				detail = record.DetailContent
				got++
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d attempt records were published", got, len(sent))
		}
	}

	steps := gjson.Get(detail, "diagnostic.route_trace.steps").Array()
	attempt := 0
	for _, step := range steps {
		if step.Get("kind").String() != "attempt" {
			continue
		}
		shape := step.Get("upstream_request_shape")
		if !shape.Exists() {
			t.Fatalf("attempt %d on %s has no shape:\n%s", attempt+1, step.Get("auth_id").String(), step.Raw)
		}
		if got := shape.Get("bytes.plain").Int(); attempt >= len(sent) || got != int64(len(sent[attempt])) {
			t.Fatalf("attempt %d: bytes.plain = %d, want the %d bytes the stub received", attempt+1, got, len(sent[attempt]))
		}
		if shape.Get("sent_headers.content_encoding").String() != "zstd" || shape.Get("client.request_kind").String() != "compaction" {
			t.Fatalf("attempt %d: shape = %s", attempt+1, shape.Raw)
		}
		if got, want := step.Get("status").Int(), int64(http.StatusBadRequest); got != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, got, want)
		}
		if len(shape.Raw) > 2048 {
			t.Fatalf("attempt %d: shape is %d bytes", attempt+1, len(shape.Raw))
		}
		t.Logf("route_trace step of attempt %d (the stub saw account %s): %s", attempt+1, sentTo[attempt], step.Raw)
		attempt++
	}
	if attempt != len(sent) {
		t.Fatalf("the trace holds %d attempts with a shape, the stub saw %d requests", attempt, len(sent))
	}
	if attempt > 1 && gjson.Get(detail, "diagnostic.route_trace.steps.0.auth_id").String() == gjson.Get(detail, "diagnostic.route_trace.steps.1.auth_id").String() {
		t.Fatal("the scheduler should have moved to another account")
	}
	if strings.Contains(detail, "first turn") || strings.Contains(detail, "next turn") || strings.Contains(detail, strings.Repeat("Z", 20)) {
		t.Fatal("the request's text or ciphertext reached detail_content through the shape")
	}
}
