package contentmoderation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func boolPtr(v bool) *bool { return &v }

// policyOf builds a normalized policy; the tests fail if its rules are invalid.
func policyOf(t *testing.T, scopeType, scopeID string, mutate func(*WordPolicy), rules ...WordRule) *WordPolicy {
	t.Helper()
	p := &WordPolicy{ScopeType: scopeType, ScopeID: scopeID, Enabled: true, InheritTenant: true, Rules: rules}
	if mutate != nil {
		mutate(p)
	}
	if err := p.Normalize(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	return p
}

func tenantOf(t *testing.T, action string, rules ...WordRule) *WordPolicy {
	return policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = action }, rules...)
}

func evalOne(t *testing.T, format sdktranslator.Format, payload string, contexts []wordContext) WordVerdict {
	t.Helper()
	return evaluateWords(format, []byte(payload), contexts, wordEvalOptions{})
}

const (
	chatBad = `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello bad world"}]}`
)

func TestWordGuardBlocksByDefault(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, "", kw("bad")), nil)
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, contexts)
	if !verdict.Blocked || verdict.Action != WordVerdictBlock || verdict.Payload != nil {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.BlockMessage != DefaultBlockMessage {
		t.Fatalf("block message = %q", verdict.BlockMessage)
	}
	if len(verdict.Matches) != 1 || verdict.Matches[0].Layer != WordScopeTenant || verdict.Matches[0].Pattern != "bad" || verdict.Matches[0].Source != "primary" {
		t.Fatalf("matches = %+v", verdict.Matches)
	}
}

func TestWordGuardBlockMessageIsConfigurable(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.BlockMessage = "no bad words here" }, kw("bad"))
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, buildWordContexts(nil, tenant, nil))
	if verdict.BlockMessage != "no bad words here" {
		t.Fatalf("block message = %q", verdict.BlockMessage)
	}
}

func TestWordGuardAllowsCleanRequests(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, "", kw("forbidden")), nil)
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, contexts)
	if verdict.Blocked || verdict.Action != WordVerdictAllow || verdict.Payload != nil || verdict.MatchCount != 0 {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestWordGuardLogOnlyLeavesTheRequestAlone(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, kw("bad")), nil)
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, contexts)
	if verdict.Blocked || verdict.Action != WordVerdictLog || verdict.Payload != nil || verdict.MatchCount != 1 {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.Matches[0].Action != WordActionLog {
		t.Fatalf("match = %+v", verdict.Matches[0])
	}
}

func TestWordGuardReplaceRewritesOnlyTheHit(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, WordActionReplace, kw("bad")), nil)
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, contexts)
	want := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello *** world"}]}`
	if verdict.Blocked || verdict.Action != WordVerdictReplace || string(verdict.Payload) != want || verdict.Replaced != 1 {
		t.Fatalf("verdict = %+v\npayload %s", verdict, verdict.Payload)
	}
}

func TestWordGuardReplacementTextIsConfigurable(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.Replacement = `[removed "x"]` }, kw("bad"))
	verdict := evalOne(t, sdktranslator.FormatOpenAI, chatBad, buildWordContexts(nil, tenant, nil))
	if !strings.Contains(string(verdict.Payload), `hello [removed \"x\"] world`) || !json.Valid(verdict.Payload) {
		t.Fatalf("payload = %s", verdict.Payload)
	}
}

// Normalization changes every offset; the replacement still lands on exactly
// the characters that matched, and every other byte stays as it was.
func TestWordGuardReplacesInTheOriginalText(t *testing.T) {
	tests := []struct {
		name    string
		content string // raw JSON string content
		rules   []WordRule
		fold    bool
		want    string // raw JSON string content after replacement
	}{
		{"zero width characters inside the word", `x ba\u200bd\u200cword y`, []WordRule{kw("badword")}, false, `x *** y`},
		{"zero width characters as raw bytes", "x ba\u200bd y", []WordRule{kw("bad")}, false, `x *** y`},
		{"full width letters", `say ＢＡＤ now`, []WordRule{kw("bad")}, false, `say *** now`},
		{"case", `say BaD now`, []WordRule{kw("bad")}, false, `say *** now`},
		{"traditional script", `這是測試軟體喔`, []WordRule{kw("测试软体")}, true, `這是***喔`},
		{"traditional is untouched when not folding", `這是測試軟體喔`, []WordRule{kw("测试软体")}, false, `這是測試軟體喔`},
		{"ligature", `an eﬃcient road`, []WordRule{kw("ffi")}, false, `an e***cient road`},
		{"whitespace runs", "say bad   \\n\\t word now", []WordRule{kw("bad word")}, false, `say *** now`},
		{"escapes elsewhere in the string are kept", `a\/b é bad \"q\" \\ end`, []WordRule{kw("bad")}, false, `a\/b é *** \"q\" \\ end`},
		{"several hits", `bad and BAD and b\u200bad`, []WordRule{kw("bad")}, false, `*** and *** and ***`},
		{"adjacent hits", `badbad`, []WordRule{kw("bad")}, false, `******`},
		{"regex", `call 13800138000 or 13900139000`, []WordRule{rx(`1[3-9]\d{9}`)}, false, `call *** or ***`},
		{"emoji before the hit", `😀😀 bad`, []WordRule{kw("bad")}, false, `😀😀 ***`},
		{"hit at the very start and end", `bad`, []WordRule{kw("bad")}, false, `***`},
		{"sharp s", `STRASSE`, []WordRule{kw("straße")}, false, `***`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
				p.Action = WordActionReplace
				p.FoldTraditionalChinese = boolPtr(tt.fold)
			}, tt.rules...)
			payload := `{"id":"req_1","messages":[{"role":"user","content":"` + tt.content + `"}],"signature":"sig==","n":1}`
			verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(nil, tenant, nil))
			want := `{"id":"req_1","messages":[{"role":"user","content":"` + tt.want + `"}],"signature":"sig==","n":1}`
			got := string(verdict.Payload)
			if verdict.Payload == nil {
				got = payload
			}
			if got != want {
				t.Fatalf("\n got  %s\n want %s\n verdict %+v", got, want, verdict)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("not valid JSON: %s", got)
			}
		})
	}
}

// The four protocols: the hit is replaced, the JSON stays valid, and ids,
// signatures, encrypted content and tool schemas are untouched.
func TestWordGuardReplaceKeepsProtocolFieldsInEveryFormat(t *testing.T) {
	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		// fields are gjson paths whose raw JSON must be identical afterwards.
		fields []string
		// changed is the path of the text that must now say "ok *** ok".
		changed string
	}{
		{
			name:   "openai chat",
			format: sdktranslator.FormatOpenAI,
			payload: `{"model":"m","messages":[{"role":"user","content":"ok bad ok"}],
				"tools":[{"type":"function","function":{"name":"bad_tool","description":"a bad description","parameters":{"type":"object","properties":{"bad":{"type":"string"}}}}}],
				"tool_choice":{"type":"function","function":{"name":"bad_tool"}},"user":"bad"}`,
			fields:  []string{"model", "tools", "tool_choice", "user"},
			changed: "messages.0.content",
		},
		{
			name:   "claude",
			format: sdktranslator.FormatClaude,
			payload: `{"model":"claude","max_tokens":10,"system":"keep","messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig-bad=="},{"type":"redacted_thinking","data":"enc-bad"}]},
				{"role":"user","content":[{"type":"text","text":"ok bad ok","cache_control":{"type":"ephemeral"}}]}],
				"tools":[{"name":"bad_tool","description":"bad","input_schema":{"type":"object"}}],"metadata":{"user_id":"bad"}}`,
			fields:  []string{"model", "system", "messages.0", "tools", "metadata"},
			changed: "messages.1.content.0.text",
		},
		{
			name:   "responses",
			format: sdktranslator.FormatOpenAIResponse,
			payload: `{"model":"m","instructions":"keep","input":[
				{"type":"reasoning","id":"rs_bad","encrypted_content":"enc-bad","summary":[]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"ok bad ok"}]}],
				"tools":[{"type":"function","name":"bad_tool","parameters":{"type":"object"}}],"previous_response_id":"resp_bad"}`,
			fields:  []string{"model", "instructions", "input.0", "tools", "previous_response_id"},
			changed: "input.1.content.0.text",
		},
		{
			name:   "gemini",
			format: sdktranslator.FormatGemini,
			payload: `{"systemInstruction":{"parts":[{"text":"keep"}]},"contents":[{"role":"user","parts":[{"text":"ok bad ok"},{"inlineData":{"mimeType":"image/png","data":"YmFk"}}]}],
				"tools":[{"functionDeclarations":[{"name":"bad_tool","description":"bad"}]}],"generationConfig":{"temperature":0.2}}`,
			fields:  []string{"systemInstruction", "contents.0.parts.1", "tools", "generationConfig"},
			changed: "contents.0.parts.0.text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contexts := buildWordContexts(nil, tenantOf(t, WordActionReplace, kw("bad")), nil)
			verdict := evalOne(t, tt.format, tt.payload, contexts)
			if verdict.Blocked || verdict.Payload == nil || verdict.Replaced != 1 {
				t.Fatalf("verdict = %+v", verdict)
			}
			if !gjson.ValidBytes(verdict.Payload) {
				t.Fatalf("not valid JSON: %s", verdict.Payload)
			}
			if got := gjson.GetBytes(verdict.Payload, tt.changed).String(); got != "ok *** ok" {
				t.Fatalf("%s = %q", tt.changed, got)
			}
			before, after := gjson.Parse(tt.payload), gjson.ParseBytes(verdict.Payload)
			for _, field := range tt.fields {
				if b, a := before.Get(field).Raw, after.Get(field).Raw; a != b || b == "" {
					t.Fatalf("%s changed:\n before %s\n after  %s", field, b, a)
				}
			}
			// Outside the one string literal the document is byte for byte the same.
			literal := gjson.Parse(tt.payload).Get(tt.changed)
			prefix := tt.payload[:literal.Index]
			suffix := tt.payload[literal.Index+len(literal.Raw):]
			if !strings.HasPrefix(string(verdict.Payload), prefix) || !strings.HasSuffix(string(verdict.Payload), suffix) {
				t.Fatalf("bytes outside the literal changed:\n%s", verdict.Payload)
			}
		})
	}
}

func TestWordGuardScanScopes(t *testing.T) {
	// The word sits in a different place in each request.
	claude := func(system, firstUser, assistant, tool, lastUser string) string {
		return fmt.Sprintf(`{"system":%q,"messages":[
			{"role":"user","content":%q},
			{"role":"assistant","content":%q},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q},{"type":"text","text":%q}]}]}`, system, firstUser, assistant, tool, lastUser)
	}
	places := map[string]string{
		"system":     claude("bad", "a", "b", "c", "d"),
		"first user": claude("s", "bad", "b", "c", "d"),
		"assistant":  claude("s", "a", "bad", "c", "d"),
		"tool":       claude("s", "a", "b", "bad", "d"),
		"last user":  claude("s", "a", "b", "c", "bad"),
	}
	caught := map[string]map[string]bool{
		WordScanLastUser:   {"last user": true},
		WordScanAllUser:    {"first user": true, "last user": true},
		WordScanWithSystem: {"system": true, "first user": true, "last user": true},
		WordScanWithTools:  {"system": true, "first user": true, "assistant": true, "tool": true, "last user": true},
	}
	for scope, want := range caught {
		for place, payload := range places {
			tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionLog; p.ScanScope = scope }, kw("bad"))
			verdict := evalOne(t, sdktranslator.FormatClaude, payload, buildWordContexts(nil, tenant, nil))
			if got := verdict.MatchCount > 0; got != want[place] {
				t.Errorf("scope %s, word in %s: caught=%v, want %v", scope, place, got, want[place])
			}
		}
	}
}

func TestWordGuardDefaultScopeIsAllUserMessages(t *testing.T) {
	payload := `{"messages":[{"role":"user","content":"bad early"},{"role":"assistant","content":"ok"},{"role":"user","content":"clean last"}]}`
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(nil, tenantOf(t, "", kw("bad")), nil))
	if !verdict.Blocked {
		t.Fatalf("a word in an earlier user message must be caught by default: %+v", verdict)
	}
}

func TestWordGuardReplaceUnderLastUserScopeLeavesHistoryAlone(t *testing.T) {
	payload := `{"messages":[{"role":"user","content":"bad early"},{"role":"assistant","content":"ok"},{"role":"user","content":"bad late"}]}`
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.ScanScope = WordScanLastUser }, kw("bad"))
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(nil, tenant, nil))
	want := `{"messages":[{"role":"user","content":"bad early"},{"role":"assistant","content":"ok"},{"role":"user","content":"*** late"}]}`
	if string(verdict.Payload) != want {
		t.Fatalf("payload = %s", verdict.Payload)
	}
}

func TestWordGuardPlatformCannotBeSwitchedOffBelow(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", nil, kw("secret"))
	payload := `{"messages":[{"role":"user","content":"a secret and a bad word"}]}`
	cases := map[string][]*WordPolicy{
		"tenant disabled":         {policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Enabled = false }, kw("bad")), nil},
		"tenant action is log":    {tenantOf(t, WordActionLog, kw("bad")), nil},
		"tenant has no policy":    {nil, nil},
		"key disabled":            {tenantOf(t, "", kw("bad")), policyOf(t, WordScopeKey, "k1", func(p *WordPolicy) { p.Enabled = false })},
		"key log-only":            {tenantOf(t, "", kw("bad")), policyOf(t, WordScopeKey, "k1", func(p *WordPolicy) { p.Action = WordActionLog })},
		"key opts out of tenant":  {tenantOf(t, "", kw("bad")), policyOf(t, WordScopeKey, "k1", func(p *WordPolicy) { p.InheritTenant = false; p.Action = WordActionLog })},
		"key replaces and scopes": {tenantOf(t, "", kw("bad")), policyOf(t, WordScopeKey, "k1", func(p *WordPolicy) { p.Action = WordActionReplace; p.ScanScope = WordScanLastUser })},
	}
	for name, layers := range cases {
		verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(platform, layers[0], layers[1]))
		if !verdict.Blocked || verdict.Matches[0].Layer != WordScopePlatform {
			t.Errorf("%s: the platform word must still block: %+v", name, verdict)
		}
	}
	// A disabled platform policy is the platform's own business.
	off := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Enabled = false }, kw("secret"))
	if verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(off, nil, nil)); verdict.Action != WordVerdictAllow {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestWordGuardLayerSettings(t *testing.T) {
	payload := `{"messages":[{"role":"user","content":"a bad word and a worse one"}]}`
	tenant := tenantOf(t, WordActionReplace, kw("bad"))
	tests := []struct {
		name     string
		platform *WordPolicy
		tenant   *WordPolicy
		key      *WordPolicy
		verdict  string
		payload  string
	}{
		{"tenant default", nil, tenant, nil, WordVerdictReplace, `{"messages":[{"role":"user","content":"a *** word and a worse one"}]}`},
		{"key adjusts the action", nil, tenant, policyOf(t, WordScopeKey, "k", func(p *WordPolicy) { p.Action = WordActionLog }), WordVerdictLog, ""},
		{"key adds a rule", nil, tenant, policyOf(t, WordScopeKey, "k", nil, kw("worse")), WordVerdictReplace, `{"messages":[{"role":"user","content":"a *** word and a *** one"}]}`},
		{"key changes the replacement", nil, tenant, policyOf(t, WordScopeKey, "k", func(p *WordPolicy) { p.Replacement = "[x]" }), WordVerdictReplace, `{"messages":[{"role":"user","content":"a [x] word and a worse one"}]}`},
		{"key without own settings inherits", nil, tenant, policyOf(t, WordScopeKey, "k", nil), WordVerdictReplace, `{"messages":[{"role":"user","content":"a *** word and a worse one"}]}`},
		{"key opts out of the tenant rules", nil, tenant, policyOf(t, WordScopeKey, "k", func(p *WordPolicy) { p.InheritTenant = false }, kw("worse")), WordVerdictReplace, `{"messages":[{"role":"user","content":"a bad word and a *** one"}]}`},
		{"key escalates to block", nil, tenant, policyOf(t, WordScopeKey, "k", func(p *WordPolicy) { p.Action = WordActionBlock }), WordVerdictBlock, ""},
		{"key alone, tenant has no policy", nil, nil, policyOf(t, WordScopeKey, "k", func(p *WordPolicy) { p.Action = WordActionReplace }, kw("worse")), WordVerdictReplace, `{"messages":[{"role":"user","content":"a bad word and a *** one"}]}`},
		{"platform replaces, tenant would block: block wins", policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionReplace }, kw("word")), tenantOf(t, "", kw("bad")), nil, WordVerdictBlock, ""},
		{"platform and tenant both replace", policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.Replacement = "<P>" }, kw("word")), tenant, nil, WordVerdictReplace, `{"messages":[{"role":"user","content":"a *** <P> and a worse one"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(tt.platform, tt.tenant, tt.key))
			if verdict.Action != tt.verdict {
				t.Fatalf("verdict = %+v, want %s", verdict, tt.verdict)
			}
			if tt.payload != "" && string(verdict.Payload) != tt.payload {
				t.Fatalf("payload = %s\n want    %s", verdict.Payload, tt.payload)
			}
			if tt.payload == "" && verdict.Payload != nil {
				t.Fatalf("payload must be untouched: %s", verdict.Payload)
			}
		})
	}
}

func TestWordGuardOverlappingReplacementsFromTwoLayers(t *testing.T) {
	// "foo bar" from the platform and "bar baz" from the tenant overlap on "bar":
	// one replacement covers the union, with the platform's text.
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.Replacement = "<P>" }, kw("foo bar"))
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.Replacement = "<T>" }, kw("bar baz"))
	verdict := evalOne(t, sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"x foo bar baz y"}]}`, buildWordContexts(platform, tenant, nil))
	if want := `{"messages":[{"role":"user","content":"x <P> y"}]}`; string(verdict.Payload) != want {
		t.Fatalf("payload = %s, want %s", verdict.Payload, want)
	}
}

func TestWordGuardFoldIsPerContext(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionLog; p.FoldTraditionalChinese = boolPtr(true) }, kw("软件"))
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionLog }, kw("软件"))
	verdict := evalOne(t, sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"我喜歡軟件"}]}`, buildWordContexts(platform, tenant, nil))
	if verdict.MatchCount != 1 || verdict.Matches[0].Layer != WordScopePlatform {
		t.Fatalf("only the folding platform context sees the Traditional text: %+v", verdict)
	}
}

func TestWordGuardUnrewritableHitsFailClosed(t *testing.T) {
	deep := strings.Repeat(`{"a":`, maxJSONTextDepth+6) + `"a bad word"` + strings.Repeat(`}`, maxJSONTextDepth+6)
	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		field   string
		why     string
	}{
		{"text that is not UTF-8", sdktranslator.FormatOpenAI, "{\"messages\":[{\"role\":\"user\",\"content\":\"a \xff bad\"}]}", "messages[0].content", whyInvalid},
		{"nested too deeply", sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":` + deep + `}]}]}`, "messages[0].content[0].input" + strings.Repeat(".a", maxJSONTextDepth), whyTooDeep},
		{"in arguments that are not UTF-8", sdktranslator.FormatOpenAI, "{\"messages\":[{\"role\":\"assistant\",\"content\":\"x\",\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"q\\\":\\\"\xff bad\\\"}\"}}]}]}", "messages[0].tool_calls[0].function.arguments.q", whyInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionReplace; p.ScanScope = WordScanWithTools }, kw("bad"))
			verdict := evalOne(t, tt.format, tt.payload, buildWordContexts(nil, tenant, nil))
			if !verdict.Blocked || verdict.Reason != WordReasonUnrewritable || verdict.Payload != nil {
				t.Fatalf("verdict = %+v", verdict)
			}
			// The client is told where, and why, and the policy's own words stay out of it.
			if verdict.Field != tt.field {
				t.Fatalf("field = %q, want %q", verdict.Field, tt.field)
			}
			if !strings.Contains(verdict.BlockMessage, tt.field) || !strings.Contains(verdict.BlockMessage, tt.why) || !strings.HasPrefix(verdict.BlockMessage, DefaultBlockMessage) {
				t.Fatalf("block message = %q", verdict.BlockMessage)
			}
			// The same hit is only logged when the action says log: nothing to rewrite.
			logTenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionLog; p.ScanScope = WordScanWithTools }, kw("bad"))
			if v := evalOne(t, tt.format, tt.payload, buildWordContexts(nil, logTenant, nil)); v.Blocked || v.MatchCount != 1 {
				t.Fatalf("log verdict = %+v", v)
			}
		})
	}
}

func TestWordGuardUnrewritableOnlyMattersInScope(t *testing.T) {
	// The unrewritable word is in a tool call; the default scope never reads it.
	payload := `{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"q\":\"bad\"}"}}]},{"role":"user","content":"hi"}]}`
	tenant := tenantOf(t, WordActionReplace, kw("bad"))
	if v := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(nil, tenant, nil)); v.Blocked || v.Action != WordVerdictAllow {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestWordGuardReportsPositionsAsWritten(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionLog }, kw("bad word"), kw("worse"))
	text := "\U0001f600 b\u200bad   WORD and worse"
	payload, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}})
	verdict := evaluateWords(sdktranslator.FormatOpenAI, payload, buildWordContexts(nil, tenant, nil), wordEvalOptions{positions: true})
	if len(verdict.Matches) != 2 {
		t.Fatalf("matches = %+v", verdict.Matches)
	}
	// The panel cuts the text with JavaScript string indexes: UTF-16 code units.
	units := utf16.Encode([]rune(text))
	cut := func(m WordMatch) string { return string(utf16.Decode(units[m.Start:m.End])) }
	first, second := verdict.Matches[0], verdict.Matches[1]
	if got := cut(first); got != "b\u200bad   WORD" || !first.NormalizedOnly {
		t.Fatalf("first match covers %q (normalized only: %v): %+v", got, first.NormalizedOnly, first)
	}
	if got := cut(second); got != "worse" || second.NormalizedOnly {
		t.Fatalf("second match covers %q (normalized only: %v): %+v", got, second.NormalizedOnly, second)
	}
}
