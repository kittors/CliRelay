package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const requestBodyTestModel = "rbody-model"

// countingExecutor counts the requests that reach an executor.
type countingExecutor struct{ calls atomic.Int64 }

func (e *countingExecutor) Identifier() string { return "rbody-provider" }

func (e *countingExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *countingExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, errors.New("not implemented")
}

func (e *countingExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}

func (e *countingExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *countingExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newRequestBodyTestServer(t *testing.T) (*Server, *countingExecutor) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{APIKeyEntries: []proxyconfig.APIKeyEntry{{Key: "rbody-key"}}},
		AuthDir:   t.TempDir(),
	}
	exec := &countingExecutor{}
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	credential := &auth.Auth{ID: "rbody-auth", Provider: exec.Identifier(), Status: auth.StatusActive}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: requestBodyTestModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	return NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"), WithRequestLoggerFactory(nil)), exec
}

// requestBodyEndpoint is one inference endpoint in the format it takes. body
// builds a request from the conversation it carries.
type requestBodyEndpoint struct {
	format string
	path   string
	// local requests come from the same machine (the Gemini CLI bridge takes no
	// others, and runs no request stages).
	local bool
	body  func(conversation string) string
	// conversation holds the text once; twice repeats the field that holds it,
	// nested repeats a field inside one turn of it.
	conversation, twice, nested string
}

const rbSecret = "sk-proj-Ab3Cd5Ef7Gh9Ij1Kl3Mn5Op7Qr9St1Uv3Wx5Yz7Ab9Cd"

func requestBodyEndpoints() []requestBodyEndpoint {
	m := requestBodyTestModel
	return []requestBodyEndpoint{
		{format: "chat", path: "/v1/chat/completions",
			body:         func(c string) string { return `{"model":"` + m + `",` + c + `}` },
			conversation: `"messages":[{"role":"user","content":"hi"}]`,
			twice:        `"messages":[{"role":"user","content":"hi"}],"messages":[{"role":"user","content":"` + rbSecret + `"}]`,
			nested:       `"messages":[{"role":"user","content":"hi","content":"` + rbSecret + `"}]`},
		{format: "responses", path: "/v1/responses",
			body:         func(c string) string { return `{"model":"` + m + `",` + c + `}` },
			conversation: `"input":"hi"`,
			twice:        `"input":"hi","input":"` + rbSecret + `"`,
			nested:       `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi","text":"` + rbSecret + `"}]}]`},
		{format: "claude", path: "/v1/messages",
			body:         func(c string) string { return `{"model":"` + m + `","max_tokens":8,` + c + `}` },
			conversation: `"messages":[{"role":"user","content":"hi"}]`,
			twice:        `"system":"hi","system":"` + rbSecret + `","messages":[{"role":"user","content":"hi"}]`,
			nested:       `"messages":[{"role":"user","content":[{"type":"text","text":"hi","text":"` + rbSecret + `"}]}]`},
		{format: "gemini", path: "/v1beta/models/" + m + ":generateContent",
			body:         func(c string) string { return `{` + c + `}` },
			conversation: `"contents":[{"role":"user","parts":[{"text":"hi"}]}]`,
			twice:        `"contents":[{"role":"user","parts":[{"text":"hi"}]}],"contents":[{"role":"user","parts":[{"text":"` + rbSecret + `"}]}]`,
			nested:       `"contents":[{"role":"user","parts":[{"text":"hi","text":"` + rbSecret + `"}]}]`},
		{format: "gemini-cli", path: "/v1internal:generateContent", local: true,
			body:         func(c string) string { return `{"model":"` + m + `",` + c + `}` },
			conversation: `"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			twice:        `"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]},"request":{"contents":[{"role":"user","parts":[{"text":"` + rbSecret + `"}]}]}`,
			nested:       `"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"contents":[{"role":"user","parts":[{"text":"` + rbSecret + `"}]}]}`},
	}
}

func sendRequestBody(server *Server, endpoint requestBodyEndpoint, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer rbody-key")
	if endpoint.local {
		req.RemoteAddr = "127.0.0.1:40000"
	}
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	return rr
}

// Every format's inference endpoint refuses a body that parsers read
// differently, before an executor is asked: the gateway reads the first of two
// members with one name, and the upstream the last, or both.
func TestInferenceEndpointsRefuseABodyParsersReadDifferently(t *testing.T) {
	server, exec := newRequestBodyTestServer(t)
	for _, endpoint := range requestBodyEndpoints() {
		t.Run(endpoint.format, func(t *testing.T) {
			before := exec.calls.Load()
			if rr := sendRequestBody(server, endpoint, "application/json", []byte(endpoint.body(endpoint.conversation))); exec.calls.Load() == before {
				t.Fatalf("a valid request did not reach the executor: %d %s", rr.Code, rr.Body.String())
			}
			for _, tc := range []struct {
				name, body, code string
			}{
				{"repeated conversation", endpoint.body(endpoint.twice), "duplicate_json_member"},
				{"repeated field in a turn", endpoint.body(endpoint.nested), "duplicate_json_member"},
				{"repeated model", withSecondModel(endpoint), "duplicate_json_member"},
				{"NaN", strings.Replace(endpoint.body(endpoint.conversation), `{`, `{"temperature":NaN,`, 1), "invalid_json"},
				{"trailing comma", strings.TrimSuffix(endpoint.body(endpoint.conversation), "}") + ",}", "invalid_json"},
			} {
				before := exec.calls.Load()
				rr := sendRequestBody(server, endpoint, "application/json", []byte(tc.body))
				if rr.Code != http.StatusBadRequest || gjson.Get(rr.Body.String(), "error.code").String() != tc.code {
					t.Errorf("%s: %d %s, want 400 %s", tc.name, rr.Code, rr.Body.String(), tc.code)
				}
				if exec.calls.Load() != before {
					t.Errorf("%s reached the executor", tc.name)
				}
				if strings.Contains(rr.Body.String(), rbSecret) {
					t.Errorf("%s: the error quotes a string value: %s", tc.name, rr.Body.String())
				}
			}
		})
	}
}

// withSecondModel is a valid request of endpoint with a second, more expensive
// "model": routing, the model gate and billing read the first, an upstream that
// takes the last runs the second.
func withSecondModel(endpoint requestBodyEndpoint) string {
	body := endpoint.body(endpoint.conversation)
	if endpoint.format == "gemini" {
		// the model is in the path; the body names none of its own
		body = strings.Replace(body, `{`, `{"model":"`+requestBodyTestModel+`",`, 1)
	}
	return strings.Replace(body, `{`, `{"model":"`+requestBodyTestModel+`-pro",`, 1)
}

// A JSON body sent as a form passes the request stages, which leave forms to
// the image edit handler, and is refused where a handler reads it as JSON.
func TestAJSONBodyLabelledAsAFormIsCheckedByItsHandler(t *testing.T) {
	server, exec := newRequestBodyTestServer(t)
	chat := requestBodyEndpoints()[0]
	rr := sendRequestBody(server, chat, "multipart/form-data; boundary=x", []byte(chat.body(chat.twice)))
	if rr.Code != http.StatusBadRequest || gjson.Get(rr.Body.String(), "error.code").String() != "duplicate_json_member" || exec.calls.Load() != 0 {
		t.Fatalf("%d %s, %d calls", rr.Code, rr.Body.String(), exec.calls.Load())
	}
}

// The check reads a body spilled to disk from the disk, and reports what it
// found the same way.
func TestRequestBodyStageReadsALargeBodyFromDisk(t *testing.T) {
	server, exec := newRequestBodyTestServer(t)
	threshold := bodyutil.RequestBodyDiskThreshold()
	bodyutil.SetRequestBodyDiskThreshold(1 << 10)
	t.Cleanup(func() { bodyutil.SetRequestBodyDiskThreshold(threshold) })
	chat := requestBodyEndpoints()[0]
	padding := `"metadata":{"note":"` + strings.Repeat("x", 4<<10) + `"},`
	body := strings.Replace(chat.body(chat.twice), `{`, `{`+padding, 1)
	rr := sendRequestBody(server, chat, "application/json", []byte(body))
	if rr.Code != http.StatusBadRequest || gjson.Get(rr.Body.String(), "error.code").String() != "duplicate_json_member" || exec.calls.Load() != 0 {
		t.Fatalf("%d %s, %d calls", rr.Code, rr.Body.String(), exec.calls.Load())
	}
	if !strings.Contains(gjson.Get(rr.Body.String(), "error.message").String(), `named "messages"`) {
		t.Fatalf("message = %s", rr.Body.String())
	}
}
