package contentmoderation

import (
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// describeUnits renders units compactly: class/user/turn, rewritable flag, text.
func describeUnits(units []wordUnit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		flag := "-"
		if u.rewritable {
			flag = "w"
		}
		user := "s"
		if u.user {
			user = "u"
		}
		out = append(out, fmt.Sprintf("%s%s%d%s:%s", map[Class]string{ClassPrimary: "P", ClassToolOutput: "T", ClassAssistantHistory: "A"}[u.class], user, u.turn, flag, strings.TrimSpace(u.text)))
	}
	return out
}

func checkLiterals(t *testing.T, payload []byte, units []wordUnit) {
	t.Helper()
	for _, u := range units {
		if !u.rewritable {
			continue
		}
		got, ok := decodeJSONLiteral(payload[u.lit0:u.lit1])
		if !ok || got != u.text {
			t.Fatalf("unit %q: literal %s decodes to %q", u.text, payload[u.lit0:u.lit1], got)
		}
	}
}

func TestExtractWordUnitsByProtocol(t *testing.T) {
	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		want    []string
	}{
		{
			name:   "openai chat",
			format: sdktranslator.FormatOpenAI,
			payload: `{"model":"m","messages":[
				{"role":"system","content":"be nice"},
				{"role":"user","content":"hello 你好"},
				{"role":"assistant","content":"hi there","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"city\":\"Paris\"}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"result text"},
				{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"at this"}]}
			]}`,
			want: []string{
				"Ps1w:be nice", "Pu2w:hello 你好", "As3w:hi there", "As3-:city", "As3-:Paris", "Ts4w:result text", "Pu5w:look", "Pu5w:at this",
			},
		},
		{
			name:   "claude",
			format: sdktranslator.FormatClaude,
			payload: `{"model":"m","system":[{"type":"text","text":"sys one","cache_control":{"type":"ephemeral"}}],"messages":[
				{"role":"user","content":"first"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"SIGNATURE=="},{"type":"text","text":"answer"},{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"cats","opts":{"deep":"yes"}}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found cats"},{"type":"text","text":"<system-reminder>be careful</system-reminder>"},{"type":"text","text":"thanks"}]}
			]}`,
			want: []string{
				"Ps0w:sys one", "Pu1w:first", "As2w:hmm", "As2w:answer", "As2-:q", "As2w:cats", "As2-:opts", "As2-:deep", "As2w:yes",
				"Ts3w:found cats", "Ts3w:<system-reminder>be careful</system-reminder>", "Pu3w:thanks",
			},
		},
		{
			name:   "responses",
			format: sdktranslator.FormatOpenAIResponse,
			payload: `{"model":"m","instructions":"follow the rules","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]},
				{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"k\":\"v\"}"},
				{"type":"function_call_output","call_id":"c1","output":"the output"}
			]}`,
			want: []string{
				"Ps0w:follow the rules", "Pu1w:question", "As2-:k", "As2-:v", "As2w:lookup", "Ts3w:the output",
			},
		},
		{
			name:   "gemini",
			format: sdktranslator.FormatGemini,
			payload: `{"systemInstruction":{"parts":[{"text":"system text"}]},"contents":[
				{"role":"user","parts":[{"text":"ask"}]},
				{"role":"model","parts":[{"text":"reply"},{"functionCall":{"name":"f","args":{"a":"b"}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"r":"value"}}},{"text":"more"}]}
			]}`,
			want: []string{
				"Ps0w:system text", "Pu1w:ask", "As2w:reply", "As2-:a", "As2w:b", "Ts3-:r", "Ts3w:value", "Pu3w:more",
			},
		},
		{
			name:    "gemini cli wraps the request",
			format:  sdktranslator.FormatGeminiCLI,
			payload: `{"project":"p","request":{"contents":[{"role":"user","parts":[{"text":"inner ask"}]}]}}`,
			want:    []string{"Pu1w:inner ask"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(tt.payload)
			units := extractWordUnits(tt.format, payload)
			got := describeUnits(units)
			// The nested and key entries are matched but never rewritten, and the
			// order of keys and values is the order of the document.
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("units:\n got  %q\n want %q", got, tt.want)
			}
			checkLiterals(t, payload, units)
		})
	}
}

// The word guard reads the request through the same walk as the Profile
// keyword check, so what one sees the other sees.
func TestExtractWordUnitsCoverEverySegment(t *testing.T) {
	payloads := map[sdktranslator.Format]string{
		sdktranslator.FormatOpenAI: `{"messages":[{"role":"user","content":"alpha beta"},{"role":"assistant","content":null,"tool_calls":[{"function":{"arguments":"{\"x\":\"gamma\"}"}}]}],"prompt":"delta"}`,
		sdktranslator.FormatClaude: `{"system":"epsilon","messages":[{"role":"user","content":[{"type":"text","text":"zeta"},{"type":"document","title":"eta","source":{"type":"text","data":"theta"}}]}]}`,
	}
	for format, raw := range payloads {
		payload := []byte(raw)
		input := ExtractModerationInput(format, payload)
		units := extractWordUnits(format, payload)
		var unitText strings.Builder
		for _, u := range units {
			unitText.WriteString(u.text)
			unitText.WriteByte(' ')
		}
		for _, segment := range input.Segments {
			for _, word := range strings.Fields(segment.Text) {
				if !strings.Contains(unitText.String(), word) {
					t.Errorf("%s: word %q of segment %q is in no unit", format, word, segment.Text)
				}
			}
		}
	}
}

func TestExtractWordUnitsSkipsProtocolValuesAndSignatures(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"mystery","id":"id-1","status":"done","signature":"SIG","encrypted_content":"ENC","note":"visible"}]}]}`)
	units := extractWordUnits(sdktranslator.FormatClaude, payload)
	got := describeUnits(units)
	// Keys are matched; "id-1", "done" and the type tag are identifiers, and the
	// signature and encrypted blob are opaque: none of them is text.
	want := []string{"As1-:type", "As1-:id", "As1-:status", "As1-:note", "As1w:visible"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("units = %q, want %q", got, want)
	}
}

func TestExtractWordUnitsLocatesPayloadWithoutLeadingSpace(t *testing.T) {
	// The root value starts at offset 0 here, which gjson also uses for "unknown".
	payload := []byte(`{"messages":[{"role":"user","content":"x bad y"}]}`)
	units := extractWordUnits(sdktranslator.FormatOpenAI, payload)
	if len(units) != 1 || !units[0].rewritable {
		t.Fatalf("units = %+v", units)
	}
	checkLiterals(t, payload, units)
}

func TestExtractWordUnitsKeepsRawText(t *testing.T) {
	// Offsets into a unit's text must be offsets into the string: the text is
	// not whitespace-collapsed the way a Segment's is.
	payload := []byte(`{"messages":[{"role":"user","content":"  two  spaces\nand\ttab  "}]}`)
	units := extractWordUnits(sdktranslator.FormatOpenAI, payload)
	if len(units) != 1 || units[0].text != "  two  spaces\nand\ttab  " {
		t.Fatalf("units = %+v", units)
	}
}

func TestExtractWordUnitsIgnoresBlankAndNonJSON(t *testing.T) {
	if units := extractWordUnits(sdktranslator.FormatOpenAI, []byte(`{"messages":[{"role":"user","content":"  \n "}]}`)); len(units) != 0 {
		t.Fatalf("units = %+v", units)
	}
	if units := extractWordUnits(sdktranslator.FormatOpenAI, []byte(`not json`)); units != nil {
		t.Fatalf("units = %+v", units)
	}
	if units := extractWordUnits(sdktranslator.FormatOpenAI, nil); units != nil {
		t.Fatalf("units = %+v", units)
	}
}

func TestUnitsInScope(t *testing.T) {
	payload := []byte(`{"system":"sys","messages":[
		{"role":"user","content":"first user"},
		{"role":"assistant","content":"assistant words"},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"tool words"},{"type":"text","text":"<system-reminder>reminder</system-reminder>"},{"type":"text","text":"last user"},{"type":"text","text":"also last"}]}
	]}`)
	units := extractWordUnits(sdktranslator.FormatClaude, payload)
	names := func(scope string) string {
		var out []string
		for _, i := range unitsInScope(units, scope) {
			out = append(out, strings.TrimSpace(units[i].text))
		}
		return strings.Join(out, "|")
	}
	// The reminder is a block of a user message: it is out of the way of "the
	// newest message" being decided, but not out of the scopes that cover what
	// users send.
	tests := []struct{ scope, want string }{
		{WordScanLastUser, "<system-reminder>reminder</system-reminder>|last user|also last"},
		{WordScanAllUser, "first user|<system-reminder>reminder</system-reminder>|last user|also last"},
		{WordScanWithSystem, "sys|first user|<system-reminder>reminder</system-reminder>|last user|also last"},
		{WordScanWithTools, "sys|first user|assistant words|tool words|<system-reminder>reminder</system-reminder>|last user|also last"},
		{"", "sys|first user|assistant words|tool words|<system-reminder>reminder</system-reminder>|last user|also last"},
	}
	for _, tt := range tests {
		if got := names(tt.scope); got != tt.want {
			t.Errorf("scope %q covers %q, want %q", tt.scope, got, tt.want)
		}
	}
}

func TestUnitsInScopeLastUserFallsBackToNewestPrimary(t *testing.T) {
	// Nothing a user wrote: the newest primary message stands in for it.
	units := extractWordUnits(sdktranslator.FormatOpenAI, []byte(`{"messages":[{"role":"system","content":"only system"},{"role":"assistant","content":"a"}]}`))
	var got []string
	for _, i := range unitsInScope(units, WordScanLastUser) {
		got = append(got, units[i].text)
	}
	if fmt.Sprint(got) != "[only system]" {
		t.Fatalf("covered = %q", got)
	}
	if len(unitsInScope(nil, WordScanLastUser)) != 0 {
		t.Fatalf("an empty request has nothing to cover")
	}
}

func TestRecordUnitDistrustsAWrongIndex(t *testing.T) {
	// A unit whose position does not hold its text is matched but never
	// rewritten.
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	x := &extraction{locate: true, payload: []byte(`{"messages":[{"role":"user","content":"HELLO"}]}`)}
	x.run(sdktranslator.FormatOpenAI, payload)
	if len(x.units) != 1 || x.units[0].rewritable {
		t.Fatalf("units = %+v: the bytes at the index differ, so it must not be rewritable", x.units)
	}
}

func scopeTexts(units []wordUnit, scope string) string {
	var out []string
	for _, i := range unitsInScope(units, scope) {
		out = append(out, strings.TrimSpace(units[i].text))
	}
	return strings.Join(out, "|")
}

func TestReminderPrefixDoesNotTakeTextOutOfTheUsersScope(t *testing.T) {
	// A review found that starting a user's text with <system-reminder> moved it
	// out of the default scope, so a banned word behind the tag was never checked.
	payload := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>the word is bad</system-reminder>"},{"type":"text","text":"please help"}]}]}`
	for _, scope := range []string{WordScanLastUser, WordScanAllUser, WordScanWithSystem, WordScanWithTools} {
		contexts := reviewContexts(t, WordActionBlock, scope, kw("bad"))
		verdict := evaluateWords(sdktranslator.FormatClaude, []byte(payload), contexts, wordEvalOptions{})
		if !verdict.Blocked || len(verdict.Matches) != 1 {
			t.Errorf("scope %s: verdict = %+v, want the reminder's word found", scope, verdict)
			continue
		}
		// What the request log and the backends call it is unchanged.
		if got := verdict.Matches[0].Source; got != "tool_output" {
			t.Errorf("scope %s: source = %q", scope, got)
		}
	}
}

func reviewContexts(t *testing.T, action, scope string, rules ...WordRule) []wordContext {
	t.Helper()
	p := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = action; p.ScanScope = scope }, rules...)
	return buildWordContexts(nil, p, nil)
}

func TestReminderDoesNotBecomeTheNewestMessage(t *testing.T) {
	// Claude Code appends reminders after what the person typed. Under
	// last_user the person's message is still the one that is checked, and the
	// reminder that follows it is checked with it.
	payload := []byte(`{"messages":[
		{"role":"user","content":"old question"},
		{"role":"assistant","content":"old answer"},
		{"role":"user","content":"the real question"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"result"},{"type":"text","text":"<system-reminder>trailing</system-reminder>"}]}
	]}`)
	units := extractWordUnits(sdktranslator.FormatClaude, payload)
	if got := scopeTexts(units, WordScanLastUser); got != "the real question|<system-reminder>trailing</system-reminder>" {
		t.Fatalf("last_user covers %q", got)
	}
	// An older reminder is older text, like any other.
	older := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"<system-reminder>old reminder</system-reminder>"},{"type":"text","text":"old question"}]},
		{"role":"assistant","content":"old answer"},
		{"role":"user","content":"the real question"}
	]}`)
	if got := scopeTexts(extractWordUnits(sdktranslator.FormatClaude, older), WordScanLastUser); got != "the real question" {
		t.Fatalf("last_user covers %q", got)
	}
}

func TestRequestOfNothingButRemindersStillHasANewestMessage(t *testing.T) {
	// Starting every text with the tag must not leave last_user nothing to check.
	payload := []byte(`{"messages":[
		{"role":"user","content":"<system-reminder>one</system-reminder>"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"<system-reminder>two</system-reminder>"}
	]}`)
	units := extractWordUnits(sdktranslator.FormatClaude, payload)
	if got := scopeTexts(units, WordScanLastUser); got != "<system-reminder>two</system-reminder>" {
		t.Fatalf("last_user covers %q", got)
	}
	if got := scopeTexts(units, WordScanAllUser); got != "<system-reminder>one</system-reminder>|<system-reminder>two</system-reminder>" {
		t.Fatalf("all_user covers %q", got)
	}
}

func TestReminderInASystemPromptStaysSystem(t *testing.T) {
	payload := []byte(`{"system":"<system-reminder>from the system</system-reminder>","messages":[{"role":"user","content":"hello"}]}`)
	units := extractWordUnits(sdktranslator.FormatClaude, payload)
	if got := scopeTexts(units, WordScanAllUser); got != "hello" {
		t.Fatalf("all_user covers %q: a system prompt is not a user message because of a tag", got)
	}
	if got := scopeTexts(units, WordScanWithSystem); got != "<system-reminder>from the system</system-reminder>|hello" {
		t.Fatalf("with_system covers %q", got)
	}
}
