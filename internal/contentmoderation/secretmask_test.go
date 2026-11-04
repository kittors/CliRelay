package contentmoderation

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func testTable(seed byte) *secretTable {
	return newSecretTable(bytes.Repeat([]byte{seed}, secretMaskingKeyBytes))
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
	table := testTable(1)
	masked, count, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, table)
	if failure != "" || masked == nil {
		t.Fatalf("masking failed: %q", failure)
	}
	if !gjson.ValidBytes(masked) {
		t.Fatalf("masked request is not JSON: %s", masked)
	}
	if count != 5 || len(table.values) != 4 {
		t.Fatalf("replaced %d secrets, %d different; want 5 and 4", count, len(table.values))
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
	key := table.placeholder(secretAPIKey, testAnthropicKey)
	if gjson.GetBytes(masked, "system.0.text").String() != "Deploy with "+key || !strings.Contains(gjson.GetBytes(masked, "messages.0.content.0.text").String(), key) {
		t.Fatalf("the same key got different placeholders: %s", masked)
	}
	// Putting the values back gives the request byte for byte: nothing else was
	// touched.
	if restored := table.restore(string(masked)); restored != string(body) {
		t.Fatalf("restored request differs:\n%s\n%s", restored, body)
	}
}

func TestMaskRequestSecretsIsStableAndPerTenant(t *testing.T) {
	body := claudeHistory()
	first, _, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testTable(1))
	again, _, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testTable(1))
	if !bytes.Equal(first, again) {
		t.Fatal("the same request masked twice differs; the upstream's prompt cache would miss every turn")
	}
	other, _, _ := maskRequestSecrets(sdktranslator.FormatClaude, body, testTable(2))
	if bytes.Equal(first, other) {
		t.Fatal("two tenants got the same placeholders for the same secret")
	}
	// A placeholder of one tenant means nothing to another's table.
	tenantA := testTable(1)
	placeholder := tenantA.placeholder(secretAPIKey, testOpenAIKey)
	tenantB := testTable(2)
	tenantB.placeholder(secretAPIKey, testAWSKey)
	if got := tenantB.restore("echo " + placeholder); got != "echo "+placeholder {
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
	table := testTable(3)
	masked, count, failure := maskRequestSecrets(sdktranslator.FormatOpenAI, body, table)
	if failure != "" || count != 1 {
		t.Fatalf("count = %d, failure = %q", count, failure)
	}
	inner := gjson.GetBytes(masked, "messages.1.tool_calls.0.function.arguments").String()
	if !gjson.Valid(inner) || strings.Contains(inner, "9f8e7d6c5b4a") || !strings.Contains(inner, "Bearer {{TOKEN_") {
		t.Fatalf("arguments = %s", inner)
	}
	if table.restore(string(masked)) != string(body) {
		t.Fatal("the arguments did not come back as they were")
	}
}

func TestMaskRequestSecretsLeavesARequestWithoutSecretsAlone(t *testing.T) {
	body := chatRequest(`set "max_tokens": 4096 and sort_key = 'name'`)
	if masked, count, failure := maskRequestSecrets(sdktranslator.FormatOpenAI, body, testTable(1)); masked != nil || count != 0 || failure != "" {
		t.Fatalf("masked = %s, count = %d, failure = %q", masked, count, failure)
	}
}

func TestMaskRequestSecretsRefusesWhatItCannotRewrite(t *testing.T) {
	// A key as an object key of a tool's input is matched but not renamed.
	body := []byte(`{"model":"claude","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"` + testOpenAIKey + `":1}}]},{"role":"user","content":"go"}]}`)
	masked, _, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, testTable(1))
	if masked != nil || failure == "" {
		t.Fatalf("masked = %s, failure = %q", masked, failure)
	}
	if strings.Contains(failure, testOpenAIKey) {
		t.Fatalf("the failure names the secret: %q", failure)
	}
}

func TestSecretReplyRestoresAWholeReply(t *testing.T) {
	table := testTable(1)
	key := table.placeholder(secretAPIKey, testOpenAIKey)
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[` +
		`{"type":"text","text":"Set ` + key + ` and retry."},` +
		`{"type":"tool_use","id":"t1","name":"bash","input":{"command":"export K=` + key + `","env":["A=` + key + `"]}}]}`
	resp := newSecretReply(table, sdktranslator.FormatClaude).FilterResponse(cliproxyexecutor.Response{Payload: []byte(body), Headers: http.Header{"Content-Length": {"1"}}})
	want := strings.ReplaceAll(body, key, testOpenAIKey)
	if string(resp.Payload) != want {
		t.Fatalf("reply = %s\nwant %s", resp.Payload, want)
	}
	if resp.Headers.Get("Content-Length") != "" {
		t.Fatal("the old Content-Length was kept")
	}
}

func runSecretStream(t *testing.T, table *secretTable, format sdktranslator.Format, chunks ...string) [][]byte {
	t.Helper()
	stream := newSecretReply(table, format).NewStream()
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
	table := testTable(1)
	key := table.placeholder(secretAPIKey, testOpenAIKey)
	cut := strings.Index(key, "_KEY") + 2
	out := runSecretStream(t, table, sdktranslator.FormatClaude,
		claudeStart, claudeBlock,
		claudeText(0, "use {"), claudeText(0, key[1:cut]), claudeText(0, key[cut:]+" now, {not a placeholder}"),
		claudeStop, claudeEnd)
	if got := readerOf(sdktranslator.FormatClaude, out); got != "use "+testOpenAIKey+" now, {not a placeholder}" {
		t.Fatalf("the client reads %q", got)
	}
}

func TestSecretReplyStreamRestoresToolArguments(t *testing.T) {
	table := testTable(1)
	key := table.placeholder(secretAPIKey, testOpenAIKey)
	half := len(key) / 2

	claude := runSecretStream(t, table, sdktranslator.FormatClaude,
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
	out := runSecretStream(t, table, sdktranslator.FormatOpenAI, chat(`{"k":"`+key[:half]), chat(key[half:]+`"}`))
	var args strings.Builder
	for _, doc := range docsOf(out) {
		args.WriteString(gjson.Get(doc, "choices.0.delta.tool_calls.0.function.arguments").String())
	}
	if args.String() != `{"k":"`+testOpenAIKey+`"}` {
		t.Fatalf("the tool gets %q", args.String())
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
	if !ok || snapshot.Masked != 1 || snapshot.Kinds["API_KEY"] != 1 {
		t.Fatalf("request log snapshot = %+v", snapshot)
	}

	// The model answers with the placeholder; the client reads its key.
	placeholder := gjson.GetBytes(result.Payload, "messages.0.content").String()[len("deploy with "):]
	reply := result.Reply.FilterResponse(cliproxyexecutor.Response{Payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Using ` + placeholder + `."}}]}`)})
	if got := gjson.GetBytes(reply.Payload, "choices.0.message.content").String(); got != "Using "+testOpenAIKey+"." {
		t.Fatalf("the client reads %q", got)
	}
}

func TestGuardRequestMasksBothBodiesWhenTheyDiffer(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	original := chatRequest("key " + testOpenAIKey)
	payload := chatRequest("translated key " + testOpenAIKey)
	req, opts := guardOptions("tenant-a", original)
	req.Payload = payload
	result := moderator.GuardRequest(context.Background(), req, opts)
	if result.Payload == nil || result.OriginalRequest == nil || sameStorage(result.Payload, result.OriginalRequest) {
		t.Fatalf("result = %+v", result)
	}
	if bytes.Contains(result.Payload, []byte(testOpenAIKey)) || bytes.Contains(result.OriginalRequest, []byte(testOpenAIKey)) {
		t.Fatal("a body kept the key")
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

func TestSecretMaskingRegistryKeepsTheKeyAcrossSwitches(t *testing.T) {
	store := newSecretStoreStub()
	registry := NewSecretMaskingRegistry()
	if _, err := registry.SetEnabled(context.Background(), "t", true); err != ErrSecretMaskingNotPersistent {
		t.Fatalf("a write without a store returned %v", err)
	}
	registry.SetStore(store)
	if got, err := registry.For("t"); err != nil || got.Enabled {
		t.Fatal("masking is on before anyone switched it on")
	}
	on, err := registry.SetEnabled(context.Background(), "t", true)
	if err != nil || !on.Enabled || on.Key == "" {
		t.Fatalf("on = %+v, %v", on, err)
	}
	if _, ok := on.placeholderKey(); !ok {
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
		if got, err := registry.For("t"); err != nil || !got.Enabled {
			t.Fatal("masking is off")
		}
	}
	if store.loads != loads {
		t.Fatalf("the cache was skipped: %d reads", store.loads-loads)
	}
	store.err = fmt.Errorf("database is down")
	registry.Invalidate("t")
	if got, err := registry.For("t"); err == nil || !got.Enabled {
		t.Fatal("a failed read switched masking off")
	}
}

// One request's executions share its reply filter; their streams restore at
// the same time. Run with -race.
func TestSecretReplyIsSafeToShareBetweenStreams(t *testing.T) {
	table := testTable(1)
	key := table.placeholder(secretAPIKey, testOpenAIKey)
	reply := newSecretReply(table, sdktranslator.FormatClaude)
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
