package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Fake credentials of the right shapes. None of them was ever issued.
const (
	testAnthropicKey = "sk-ant-api03-Zx8Qm2Lp9Vn4Rt6Yb1Kc3Hd5Jf7Gs0Aw2Ee4Ii6Oo8Uu"
	testOpenAIKey    = "sk-proj-Ab3Cd5Ef7Gh9Ij1Kl3Mn5Op7Qr9St1Uv3Wx5Yz7Ab9Cd"
	testAWSKey       = "AKIAQ3EXAMPLE7Z4K2P9"
)

func testMasker(seed byte) *privacymask.Masker {
	return privacymask.NewMasker(bytes.Repeat([]byte{seed}, secretMaskingKeyBytes), privacymask.Options{Secrets: true}, nil, "")
}

// placeholderOf is the placeholder m gives the first value it finds in text.
func placeholderOf(t *testing.T, m *privacymask.Masker, text string) string {
	t.Helper()
	edits := m.Plan([]string{text}, nil)[0]
	if len(edits) == 0 {
		t.Fatalf("nothing found in %q", text)
	}
	return edits[0].Text
}

// claudeHistory is a Claude Code turn: a key in the system prompt, in the
// user's text, in a tool call the model made and in what the tool returned,
// and in a signed thinking block that must reach the upstream as it came.
func claudeHistory() []byte {
	return []byte(`{"model":"claude-sonnet","system":[{"type":"text","text":"Deploy with ` + testAnthropicKey + `"}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"my key is ` + testAnthropicKey + `, the db is postgres://app:Tr0ub4dor3@db:5432/app"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"they pasted ` + testAnthropicKey + `","signature":"c2lnbmF0dXJl"},` +
		`{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"export OPENAI_API_KEY=` + testOpenAIKey + `"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"aws key ` + testAWSKey + `"}]}]}`)
}

func TestMaskRequestSecretsReplacesEveryCopyAndKeepsTheRest(t *testing.T) {
	body := claudeHistory()
	masker := testMasker(1)
	masked, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, masker, false)
	if failure != "" || masked == nil {
		t.Fatalf("masking failed: %q", failure)
	}
	if !gjson.ValidBytes(masked) {
		t.Fatalf("masked request is not JSON: %s", masked)
	}
	if masker.Places() != 5 || len(masker.Values()) != 4 {
		t.Fatalf("replaced %d places, %d different values; want 5 and 4", masker.Places(), len(masker.Values()))
	}
	for _, secret := range []string{testOpenAIKey, testAWSKey, "Tr0ub4dor3"} {
		if bytes.Contains(masked, []byte(secret)) {
			t.Fatalf("%q reached the upstream: %s", secret, masked)
		}
	}
	// The signed thinking block is the only place the key is left.
	if got := bytes.Count(masked, []byte(testAnthropicKey)); got != 1 || gjson.GetBytes(masked, "messages.1.content.0.thinking").String() != "they pasted "+testAnthropicKey {
		t.Fatalf("the key is left %d times; thinking = %q", got, gjson.GetBytes(masked, "messages.1.content.0.thinking").String())
	}
	key := placeholderOf(t, testMasker(1), testAnthropicKey)
	if gjson.GetBytes(masked, "system.0.text").String() != "Deploy with "+key || !strings.Contains(gjson.GetBytes(masked, "messages.0.content.0.text").String(), key) {
		t.Fatalf("the same key got different placeholders: %s", masked)
	}
	// Putting the values back gives the request byte for byte: nothing else was
	// touched.
	if restored := masker.Restorer().Restore(string(masked), false); restored != string(body) {
		t.Fatalf("restored request differs:\n%s\n%s", restored, body)
	}
}

func TestMaskRequestSecretsIsStableAndPerTenant(t *testing.T) {
	body := claudeHistory()
	first, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testMasker(1), false)
	again, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testMasker(1), false)
	if !bytes.Equal(first, again) {
		t.Fatal("the same request masked twice differs; the upstream's prompt cache would miss every turn")
	}
	other, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testMasker(2), false)
	if bytes.Equal(first, other) {
		t.Fatal("two tenants got the same placeholders for the same secret")
	}
	// A placeholder of one tenant means nothing to another's reply.
	placeholder := placeholderOf(t, testMasker(1), testOpenAIKey)
	tenantB := testMasker(2)
	placeholderOf(t, tenantB, testAWSKey)
	if got := tenantB.Restorer().Restore("echo "+placeholder, false); got != "echo "+placeholder {
		t.Fatalf("tenant B's reply revealed %q", got)
	}
	if !strings.HasPrefix(placeholder, "{{API_KEY_") || !strings.HasSuffix(placeholder, "}}") {
		t.Fatalf("placeholder = %q", placeholder)
	}
}

func TestMaskRequestSecretsReachesToolCallArguments(t *testing.T) {
	arguments := `{"cmd":"curl -H 'Authorization: Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0' https://api.internal"}`
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"run it"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":` + quoteJSON(arguments) + `}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	masker := testMasker(3)
	masked, failure := maskRequestSecrets(sdktranslator.FormatOpenAI, body, masker, false)
	if failure != "" || masker.Places() != 1 {
		t.Fatalf("places = %d, failure = %q", masker.Places(), failure)
	}
	inner := gjson.GetBytes(masked, "messages.1.tool_calls.0.function.arguments").String()
	if !gjson.Valid(inner) || strings.Contains(inner, "9f8e7d6c5b4a") || !strings.Contains(inner, "Bearer {{TOKEN_") {
		t.Fatalf("arguments = %s", inner)
	}
	if masker.Restorer().Restore(string(masked), false) != string(body) {
		t.Fatal("the arguments did not come back as they were")
	}
}

// A value found anywhere in a request is masked everywhere in it: the password
// a rule finds in DB_PASSWORD=… is also hidden in the tool call that has it
// bare, where no rule would find it.
func TestMaskRequestSecretsMasksAKnownValueWhereNoRuleWould(t *testing.T) {
	body := chatRequestWithToolCall("my .env has DB_PASSWORD=Qx7mP2vL9kR4", `{"cmd":"psql -h db -W Qx7mP2vL9kR4"}`)
	masker := testMasker(4)
	masked, failure := maskRequestSecrets(sdktranslator.FormatOpenAI, body, masker, false)
	if failure != "" || bytes.Contains(masked, []byte("Qx7mP2vL9kR4")) {
		t.Fatalf("failure = %q, masked = %s", failure, masked)
	}
	if v := masker.Values(); len(v) != 1 || v[0].N != 2 {
		t.Fatalf("values = %+v", v)
	}
}

func chatRequestWithToolCall(text, arguments string) []byte {
	return []byte(`{"model":"gpt","messages":[{"role":"user","content":` + quoteJSON(text) + `},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":` + quoteJSON(arguments) + `}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
}

func TestMaskRequestSecretsLeavesARequestWithoutSecretsAlone(t *testing.T) {
	body := chatRequest(`set "max_tokens": 4096 and sort_key = 'name'`)
	masker := testMasker(1)
	if masked, failure := maskRequestSecrets(sdktranslator.FormatOpenAI, body, masker, false); masked != nil || masker.Places() != 0 || failure != "" {
		t.Fatalf("masked = %s, places = %d, failure = %q", masked, masker.Places(), failure)
	}
}

func TestMaskRequestSecretsRefusesWhatItCannotRewrite(t *testing.T) {
	// A key as an object key of a tool's input is matched but not renamed.
	body := []byte(`{"model":"claude","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"` + testOpenAIKey + `":1}}]},{"role":"user","content":"go"}]}`)
	masked, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, testMasker(1), false)
	if masked != nil || failure == "" {
		t.Fatalf("masked = %s, failure = %q", masked, failure)
	}
	if strings.Contains(failure, testOpenAIKey) {
		t.Fatalf("the failure names the secret: %q", failure)
	}
}

func TestSecretReplyRestoresAWholeReply(t *testing.T) {
	masker := testMasker(1)
	key := placeholderOf(t, masker, testOpenAIKey)
	// the model wrote it back once with spaces inside the braces
	loose := "{{ " + strings.Trim(key, "{}") + " }}"
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[` +
		`{"type":"text","text":"Set ` + key + ` and retry, or ` + loose + `."},` +
		`{"type":"tool_use","id":"t1","name":"bash","input":{"command":"export K=` + key + `","env":["A=` + key + `"]}}]}`
	resp := newSecretReply(masker.Restorer(), sdktranslator.FormatClaude).FilterResponse(cliproxyexecutor.Response{Payload: []byte(body), Headers: http.Header{"Content-Length": {"1"}}})
	want := strings.ReplaceAll(strings.ReplaceAll(body, loose, testOpenAIKey), key, testOpenAIKey)
	if string(resp.Payload) != want {
		t.Fatalf("reply = %s\nwant %s", resp.Payload, want)
	}
	if resp.Headers.Get("Content-Length") != "" {
		t.Fatal("the old Content-Length was kept")
	}
}

func runSecretStream(t *testing.T, restorer *privacymask.Restorer, format sdktranslator.Format, chunks ...string) [][]byte {
	t.Helper()
	stream := newSecretReply(restorer, format).NewStream()
	var out [][]byte
	for _, chunk := range chunks {
		out = append(out, stream.Push([]byte(chunk))...)
	}
	out = append(out, stream.Flush()...)
	for _, doc := range docsOf(out) {
		if !gjson.Valid(doc) {
			t.Fatalf("invalid JSON reached the client: %q", doc)
		}
	}
	return out
}

func TestSecretReplyStreamJoinsAPlaceholderSplitAcrossEvents(t *testing.T) {
	masker := testMasker(1)
	key := placeholderOf(t, masker, testOpenAIKey)
	cut := strings.Index(key, "_KEY") + 2
	out := runSecretStream(t, masker.Restorer(), sdktranslator.FormatClaude,
		claudeStart, claudeBlock,
		claudeText(0, "use {"), claudeText(0, key[1:cut]), claudeText(0, key[cut:]+" now, {not a placeholder}"),
		claudeStop, claudeEnd)
	if got := readerOf(sdktranslator.FormatClaude, out); got != "use "+testOpenAIKey+" now, {not a placeholder}" {
		t.Fatalf("the client reads %q", got)
	}
}

func TestSecretReplyStreamRestoresToolArguments(t *testing.T) {
	masker := testMasker(1)
	key := placeholderOf(t, masker, testOpenAIKey)
	half := len(key) / 2

	claude := runSecretStream(t, masker.Restorer(), sdktranslator.FormatClaude,
		claudeStart,
		claudeEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"bash","input":{}}}`),
		claudeEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+quoteJSON(`{"command":"export K=`+key[:half])+`}}`),
		claudeEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+quoteJSON(key[half:]+`"}`)+`}}`),
		claudeEnd)
	var input strings.Builder
	for _, doc := range docsOf(claude) {
		input.WriteString(gjson.Get(doc, "delta.partial_json").String())
	}
	if input.String() != `{"command":"export K=`+testOpenAIKey+`"}` {
		t.Fatalf("the tool gets %q", input.String())
	}

	chat := func(arguments string) string {
		return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`, quoteJSON(arguments))
	}
	out := runSecretStream(t, masker.Restorer(), sdktranslator.FormatOpenAI, chat(`{"k":"`+key[:half]), chat(key[half:]+`"}`))
	var args strings.Builder
	for _, doc := range docsOf(out) {
		args.WriteString(gjson.Get(doc, "choices.0.delta.tool_calls.0.function.arguments").String())
	}
	if args.String() != `{"k":"`+testOpenAIKey+`"}` {
		t.Fatalf("the tool gets %q", args.String())
	}
}

// A value that JSON escapes goes back escaped inside a tool call's arguments,
// which stay JSON, and as written in plain text.
func TestSecretReplyEscapesAValueInToolArguments(t *testing.T) {
	masker := privacymask.NewMasker(bytes.Repeat([]byte{5}, secretMaskingKeyBytes), privacymask.Options{Words: []string{`C:\vault "main"`}}, nil, "")
	p := placeholderOf(t, masker, `open C:\vault "main"`)
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"see ` + p + `","tool_calls":[{"id":"c","type":"function","function":{"name":"open","arguments":` + quoteJSON(`{"path":"`+p+`"}`) + `}}]}}]}`
	resp := newSecretReply(masker.Restorer(), sdktranslator.FormatOpenAI).FilterResponse(cliproxyexecutor.Response{Payload: []byte(body)})
	args := gjson.GetBytes(resp.Payload, "choices.0.message.tool_calls.0.function.arguments").String()
	if !gjson.Valid(args) || gjson.Get(args, "path").String() != `C:\vault "main"` {
		t.Fatalf("arguments = %s", args)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != `see C:\vault "main"` {
		t.Fatalf("content = %q", got)
	}
}

// secretStoreStub keeps settings in memory.
type secretStoreStub struct {
	mu       sync.Mutex
	settings map[string]SecretMaskingSettings
	versions map[string]int64
	loads    int
	err      error
}

func newSecretStoreStub() *secretStoreStub {
	return &secretStoreStub{settings: map[string]SecretMaskingSettings{}, versions: map[string]int64{}}
}

func (s *secretStoreStub) Load(_ context.Context, tenantID string) (SecretMaskingSettings, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.err != nil {
		return SecretMaskingSettings{}, 0, s.err
	}
	return s.settings[tenantID], s.versions[tenantID], nil
}

func (s *secretStoreStub) Save(_ context.Context, tenantID string, settings SecretMaskingSettings, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[tenantID] != expected {
		return ErrSecretMaskingConflict
	}
	s.settings[tenantID] = settings
	s.versions[tenantID]++
	return nil
}

// bulkStoreStub can also read every tenant at once.
type bulkStoreStub struct{ *secretStoreStub }

func (s bulkStoreStub) LoadAll(context.Context) (map[string]SecretMaskingSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]SecretMaskingSettings, len(s.settings))
	for k, v := range s.settings {
		out[k] = v
	}
	return out, nil
}

func maskingModerator(t *testing.T, enabledFor ...string) *RuntimeModerator {
	t.Helper()
	registry := NewSecretMaskingRegistry()
	registry.SetStore(newSecretStoreStub())
	for _, tenantID := range enabledFor {
		if _, err := registry.SetEnabled(context.Background(), tenantID, true); err != nil {
			t.Fatal(err)
		}
	}
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	return moderator
}

func TestGuardRequestMasksOnlyForTenantsThatAskedForIt(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	body := chatRequest("deploy with " + testOpenAIKey)

	req, opts := guardOptions("tenant-b", body)
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Payload != nil || result.Reply != nil {
		t.Fatalf("tenant-b never switched masking on, got %+v", result)
	}

	ctx, ginCtx := snapshotContext(t)
	req, opts = guardOptions("tenant-a", body)
	result := moderator.GuardRequest(ctx, req, opts)
	if result.Blocked || result.Payload == nil || result.Reply == nil {
		t.Fatalf("result = %+v", result)
	}
	if !sameStorage(result.Payload, result.OriginalRequest) {
		t.Fatal("one body came back as two; the executors would translate it twice")
	}
	if bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatalf("the key reached the upstream: %s", result.Payload)
	}
	snapshot, ok := SecretMaskingSnapshotFromGin(ginCtx)
	if !ok || snapshot.Masked != 1 || snapshot.Places != 1 || snapshot.Kinds["API_KEY"] != 1 {
		t.Fatalf("request log snapshot = %+v", snapshot)
	}

	// The model answers with the placeholder; the client reads its key.
	placeholder := gjson.GetBytes(result.Payload, "messages.0.content").String()[len("deploy with "):]
	reply := result.Reply.FilterResponse(cliproxyexecutor.Response{Payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Using ` + placeholder + `."}}]}`)})
	if got := gjson.GetBytes(reply.Payload, "choices.0.message.content").String(); got != "Using "+testOpenAIKey+"." {
		t.Fatalf("the client reads %q", got)
	}
	// The request log says what was masked and how often it came back, never
	// the value or its placeholder.
	logged, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logged, []byte(testOpenAIKey)) || bytes.Contains(logged, []byte(placeholder)) ||
		gjson.GetBytes(logged, "restored").Int() != 1 || gjson.GetBytes(logged, "values.0.head").String() != "sk-" {
		t.Fatalf("request log = %s", logged)
	}
}

func TestGuardRequestMasksBothBodiesWhenTheyDiffer(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	original := chatRequest("key " + testOpenAIKey)
	payload := chatRequest("translated key " + testOpenAIKey)
	ctx, ginCtx := snapshotContext(t)
	req, opts := guardOptions("tenant-a", original)
	req.Payload = payload
	result := moderator.GuardRequest(ctx, req, opts)
	if result.Payload == nil || result.OriginalRequest == nil || sameStorage(result.Payload, result.OriginalRequest) {
		t.Fatalf("result = %+v", result)
	}
	if bytes.Contains(result.Payload, []byte(testOpenAIKey)) || bytes.Contains(result.OriginalRequest, []byte(testOpenAIKey)) {
		t.Fatal("a body kept the key")
	}
	// one request, though two copies of it were masked
	if snapshot, _ := SecretMaskingSnapshotFromGin(ginCtx); snapshot == nil || snapshot.Places != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGuardRequestRefusesARequestItCannotMask(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	body := []byte(`{"model":"claude","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"` + testOpenAIKey + `":1}}]},{"role":"user","content":"go"}]}`)
	req, opts := guardOptions("tenant-a", body)
	opts.SourceFormat = sdktranslator.FormatClaude
	result := moderator.GuardRequest(context.Background(), req, opts)
	if !result.Blocked || result.HTTPStatus != http.StatusBadRequest || strings.Contains(result.Message, testOpenAIKey) {
		t.Fatalf("result = %+v", result)
	}
}

// Masking that cannot finish does not send the request; the word guard
// failing does not stop masking.
func TestGuardRequestFailsClosedForMasking(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	secretMaskingTestHook = func() { panic("boom") }
	defer func() { secretMaskingTestHook = nil }()
	req, opts := guardOptions("tenant-a", chatRequest("deploy with "+testOpenAIKey))
	result := moderator.GuardRequest(context.Background(), req, opts)
	if !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("a panicking masker let the request through: %+v", result)
	}
	secretMaskingTestHook = nil

	panicky := guardModerator(panickingWords{})
	panicky.secretMasking = moderator.secretMasking
	result = panicky.GuardRequest(context.Background(), req, opts)
	if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatalf("after the word guard panicked: %+v", result)
	}
}

type panickingWords struct{}

func (panickingWords) LoadWordPolicies(context.Context, string, string, string) (WordPolicies, error) {
	panic("word policies")
}

// A tenant whose settings were never read, and cannot be read now, may have
// masking on: its request is refused, not sent as it is. Once every tenant's
// settings were read at once, one without a row is known to have it off.
func TestGuardRequestRefusesWhileSettingsAreUnknown(t *testing.T) {
	store := newSecretStoreStub()
	registry := NewSecretMaskingRegistry()
	registry.SetStore(store)
	store.err = fmt.Errorf("database is down")
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	req, opts := guardOptions("tenant-x", chatRequest("hello"))
	if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("unknown settings let the request through: %+v", result)
	}

	bulk := bulkStoreStub{newSecretStoreStub()}
	registry = NewSecretMaskingRegistry()
	registry.SetStore(bulk)
	if _, err := registry.SetEnabled(context.Background(), "tenant-a", true); err != nil {
		t.Fatal(err)
	}
	registry.SetStore(bulk) // a restart: nothing cached
	if err := registry.Preload(context.Background()); err != nil {
		t.Fatal(err)
	}
	bulk.err = fmt.Errorf("database is down")
	moderator.secretMasking = registry
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked {
		t.Fatalf("a tenant with no row was refused after the preload: %+v", result)
	}
	req, opts = guardOptions("tenant-a", chatRequest("deploy with "+testOpenAIKey))
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatalf("the preloaded tenant was not masked: %+v", result)
	}
}

// What one request of a sender masked is masked in its next one, where no
// rule would find it; turning masking off forgets it.
func TestGuardRequestRemembersAcrossRequests(t *testing.T) {
	moderator := maskingModerator(t, "tenant-m")
	req, opts := guardOptions("tenant-m", chatRequest("DB_PASSWORD=Qx7mP2vL9kR4"))
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Payload == nil {
		t.Fatalf("result = %+v", result)
	}
	req, opts = guardOptions("tenant-m", chatRequest("now run psql -W Qx7mP2vL9kR4"))
	result := moderator.GuardRequest(context.Background(), req, opts)
	if result.Payload == nil || bytes.Contains(result.Payload, []byte("Qx7mP2vL9kR4")) {
		t.Fatalf("the remembered password went upstream: %+v", result)
	}
	if _, err := moderator.secretMasking.SetEnabled(context.Background(), "tenant-m", false); err != nil {
		t.Fatal(err)
	}
	if _, err := moderator.secretMasking.SetEnabled(context.Background(), "tenant-m", true); err != nil {
		t.Fatal(err)
	}
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Payload != nil {
		t.Fatalf("switching masking off kept what it remembered: %s", result.Payload)
	}
}

func TestSecretMaskingRegistryKeepsTheKeyAcrossSwitches(t *testing.T) {
	store := newSecretStoreStub()
	registry := NewSecretMaskingRegistry()
	if _, err := registry.SetEnabled(context.Background(), "t", true); err != ErrSecretMaskingNotPersistent {
		t.Fatalf("a write without a store returned %v", err)
	}
	registry.SetStore(store)
	if got := registry.For("t"); got.Enabled {
		t.Fatal("masking is on before anyone switched it on")
	}
	on, err := registry.SetEnabled(context.Background(), "t", true)
	if err != nil || !on.Enabled || on.Key == "" {
		t.Fatalf("on = %+v, %v", on, err)
	}
	if _, ok := on.placeholderKey("t"); !ok {
		t.Fatalf("the key %q is not usable", on.Key)
	}
	off, err := registry.SetEnabled(context.Background(), "t", false)
	if err != nil || off.Enabled || off.Key != on.Key {
		t.Fatalf("off = %+v, %v; the key must survive", off, err)
	}
	again, _ := registry.SetEnabled(context.Background(), "t", true)
	if again.Key != on.Key {
		t.Fatal("switching masking back on made a new key; old conversations would lose their prompt cache")
	}
	if store.versions["t"] != 3 {
		t.Fatalf("version = %d, want 3 writes", store.versions["t"])
	}
	// Writing what is stored writes nothing.
	if _, err := registry.SetEnabled(context.Background(), "t", true); err != nil || store.versions["t"] != 3 {
		t.Fatalf("an unchanged switch was written again: version %d, %v", store.versions["t"], err)
	}
}

func TestSecretMaskingRegistryCachesAndKeepsTheLastReadOnFailure(t *testing.T) {
	store := newSecretStoreStub()
	registry := NewSecretMaskingRegistry()
	registry.SetStore(store)
	if _, err := registry.SetEnabled(context.Background(), "t", true); err != nil {
		t.Fatal(err)
	}
	loads := store.loads
	for range 3 {
		if !registry.For("t").Enabled {
			t.Fatal("masking is off")
		}
	}
	if store.loads != loads {
		t.Fatalf("the cache was skipped: %d reads", store.loads-loads)
	}
	store.err = fmt.Errorf("database is down")
	registry.Invalidate("t")
	if settings, known := registry.Lookup("t"); !settings.Enabled || !known {
		t.Fatal("a failed read switched masking off")
	}
}

// One request's executions share its reply filter; their streams restore at
// the same time. Run with -race.
func TestSecretReplyIsSafeToShareBetweenStreams(t *testing.T) {
	masker := testMasker(1)
	key := placeholderOf(t, masker, testOpenAIKey)
	reply := newSecretReply(masker.Restorer(), sdktranslator.FormatClaude)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream := reply.NewStream()
			var out [][]byte
			out = append(out, stream.Push([]byte(claudeText(0, "k="+key)))...)
			out = append(out, stream.Flush()...)
			if got := readerOf(sdktranslator.FormatClaude, out); got != "k="+testOpenAIKey {
				t.Errorf("the client reads %q", got)
			}
		}()
	}
	wg.Wait()
}
