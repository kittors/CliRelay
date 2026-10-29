package contentmoderation

import (
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// extracted is a request's text per class, segments joined by spaces.
type extracted struct {
	primary, tool, assistant string
}

func extractByClass(format sdktranslator.Format, body string) extracted {
	var parts [classCount][]string
	for _, segment := range ExtractModerationInput(format, []byte(body)).Segments {
		parts[segment.Class] = append(parts[segment.Class], segment.Text)
	}
	return extracted{
		primary:   strings.Join(parts[ClassPrimary], " "),
		tool:      strings.Join(parts[ClassToolOutput], " "),
		assistant: strings.Join(parts[ClassAssistantHistory], " "),
	}
}

type extractorCase struct {
	name   string
	format sdktranslator.Format
	body   string
	want   extracted
}

func runExtractorCases(t *testing.T, tests []extractorCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractByClass(tt.format, tt.body); got != tt.want {
				t.Fatalf("extracted = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExtractModerationInputCallerTurns(t *testing.T) {
	runExtractorCases(t, []extractorCase{
		{"openai chat", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","content":"old"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`, extracted{primary: "hello world", assistant: "old"}},
		{"openai image prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-image-2","prompt":"bad word"}`, extracted{primary: "bad word"}},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"old"}]},{"role":"user","content":[{"type":"input_text","text":"response text"}]}]}`, extracted{primary: "response text", assistant: "old"}},
		{"claude", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"claude text"}]}]}`, extracted{primary: "claude text"}},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"gemini text"}]}]}`, extracted{primary: "gemini text"}},
		{"openai empty body", sdktranslator.FormatOpenAI, `{}`, extracted{}},
		{"openai no prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-image-2"}`, extracted{}},
		{"invalid json", sdktranslator.FormatOpenAI, `{"messages":`, extracted{}},

		// Every caller-controlled turn must be moderated, not only the newest one.
		{"openai all user turns", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"generated"},{"role":"user","content":"second"}]}`, extracted{primary: "first second", assistant: "generated"}},
		{"openai trailing empty user does not hide earlier turn", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"banned"},{"role":"user","content":""}]}`, extracted{primary: "banned"}},
		{"openai trailing image-only user does not hide earlier turn", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"banned"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.invalid/a.png"}}]}]}`, extracted{primary: "banned"}},
		{"openai system role is moderated", sdktranslator.FormatOpenAI, `{"messages":[{"role":"system","content":"banned"},{"role":"user","content":"hi"}]}`, extracted{primary: "banned hi"}},
		{"claude system field is moderated", sdktranslator.FormatClaude, `{"system":"banned","messages":[{"role":"user","content":"hi"}]}`, extracted{primary: "banned hi"}},
		{"responses all user turns", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"user","content":[{"type":"input_text","text":"banned"}]},{"role":"assistant","content":[{"type":"output_text","text":"generated"}]},{"role":"user","content":[{"type":"input_text","text":""}]}]}`, extracted{primary: "banned", assistant: "generated"}},
		// Instructions are the system prompt and come first in conversation order.
		{"responses instructions are moderated", sdktranslator.FormatOpenAIResponse, `{"instructions":"banned","input":"hi"}`, extracted{primary: "banned hi"}},
		{"responses prompt variables are moderated", sdktranslator.FormatOpenAIResponse, `{"prompt":{"id":"pmpt_1","variables":{"topic":"banned"}},"input":"hi"}`, extracted{primary: "banned hi"}},
		{"gemini all user turns", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"banned"}]},{"role":"model","parts":[{"text":"generated"}]},{"role":"user","parts":[{"text":""}]}]}`, extracted{primary: "banned", assistant: "generated"}},
		{"gemini system instruction is moderated", sdktranslator.FormatGemini, `{"systemInstruction":{"parts":[{"text":"banned"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, extracted{primary: "banned hi"}},
		{"gemini snake_case system instruction is moderated", sdktranslator.FormatGemini, `{"system_instruction":{"parts":[{"text":"banned"}]},"contents":[{"parts":[{"text":"hi"}]}]}`, extracted{primary: "banned hi"}},
		{"gemini cli request wrapper", sdktranslator.FormatGeminiCLI, `{"model":"gemini-2.5-pro","project":"p","request":{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"banned"}]}]}}`, extracted{primary: "sys banned"}},
		// The legacy completions prompt may be a list; token-id arrays carry no text.
		{"completions array prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-3.5-turbo-instruct","prompt":["first","banned"]}`, extracted{primary: "first banned"}},
		{"completions token prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-3.5-turbo-instruct","prompt":[1,2,3]}`, extracted{}},
		{"completions suffix", sdktranslator.FormatOpenAI, `{"prompt":"def f():","suffix":"banned"}`, extracted{primary: "def f(): banned"}},
	})
}

// Text the model reads that nobody typed as prose: each of these used to be
// skipped entirely, so it was a way around moderation.
func TestExtractModerationInputToolOutput(t *testing.T) {
	runExtractorCases(t, []extractorCase{
		{"claude tool_result string", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"banned output"},{"type":"text","text":"go on"}]}]}`, extracted{primary: "go on", tool: "banned output"}},
		{"claude tool_result blocks", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line one"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo"}},{"type":"text","text":"banned"}]}]}]}`, extracted{tool: "line one banned"}},
		{"claude document text source", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"document","title":"Doc","source":{"type":"text","media_type":"text/plain","data":"banned text"}},{"type":"text","text":"summarize"}]}]}`, extracted{primary: "summarize", tool: "Doc banned text"}},
		{"claude document content source", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"banned chunk"}]}}]}]}`, extracted{tool: "banned chunk"}},
		{"claude document pdf has no text", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}]}]}`, extracted{}},
		{"claude search result", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.invalid","title":"Result","content":[{"type":"text","text":"banned"}]}]}]}`, extracted{tool: "Result banned"}},
		{"claude system reminder is tool output", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"real"},{"role":"user","content":"<system-reminder>banned</system-reminder>"}]}`, extracted{primary: "real", tool: "<system-reminder>banned</system-reminder>"}},
		{"openai tool role", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"c1","content":"banned output"}]}`, extracted{primary: "q", tool: "banned output"}},
		{"openai tool role blocks", sdktranslator.FormatOpenAI, `{"messages":[{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"banned output"}]}]}`, extracted{tool: "banned output"}},
		{"openai legacy function role", sdktranslator.FormatOpenAI, `{"messages":[{"role":"function","name":"lookup","content":"banned output"}]}`, extracted{tool: "banned output"}},
		{"openai unknown role", sdktranslator.FormatOpenAI, `{"messages":[{"role":"ipython","content":"banned output"}]}`, extracted{tool: "banned output"}},
		{"responses function_call_output string", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call_output","call_id":"c1","output":"banned output"}]}`, extracted{tool: "banned output"}},
		{"responses function_call_output blocks", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"banned output"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo"}]}]}`, extracted{tool: "banned output"}},
		{"responses custom_tool_call_output", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":"banned output"}]}`, extracted{tool: "banned output"}},
		{"responses shell output object list", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"shell_call_output","call_id":"c1","output":[{"stdout":"banned","stderr":"","outcome":{"type":"exit","exit_code":0}}]}]}`, extracted{tool: "stdout banned stderr outcome type exit exit_code"}},
		{"gemini functionResponse", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":{"items":["banned",{"note":"x"}]}}}}]}]}`, extracted{tool: "result items banned note x"}},
		{"gemini function_response snake_case", sdktranslator.FormatGemini, `{"contents":[{"role":"function","parts":[{"function_response":{"name":"lookup","response":{"banned key":1}}}]}]}`, extracted{tool: "banned key"}},
		{"gemini code execution result", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"banned"}}]}]}`, extracted{tool: "banned"}},
	})
}

// The client writes the history it replays, so a fabricated assistant turn is
// caller input like any other.
func TestExtractModerationInputAssistantHistory(t *testing.T) {
	runExtractorCases(t, []extractorCase{
		{"openai assistant content", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","content":"banned"},{"role":"user","content":"continue"}]}`, extracted{primary: "continue", assistant: "banned"}},
		// Arguments are decoded: escaped as JSON, the keyword would not match.
		{"openai assistant tool call arguments", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"\\u4f60\\u597d\"}"}}]}]}`, extracted{assistant: "cmd 你好"}},
		{"openai assistant refusal", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","refusal":"banned"}]}`, extracted{assistant: "banned"}},
		{"claude assistant blocks", sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"banned"},{"type":"tool_use","id":"t1","name":"run","input":{"cmd":"x"}}]}]}`, extracted{assistant: "plan banned cmd x"}},
		{"claude redacted thinking has no text", sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, extracted{}},
		{"responses function_call", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call","call_id":"c1","name":"run","arguments":"{\"cmd\":\"banned\"}"}]}`, extracted{assistant: "cmd banned run"}},
		{"responses custom_tool_call", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"custom_tool_call","call_id":"c1","name":"patch","input":"banned"}]}`, extracted{assistant: "patch banned"}},
		{"responses reasoning summary", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"banned"}],"encrypted_content":"opaque"}]}`, extracted{assistant: "banned"}},
		{"gemini model turn", sdktranslator.FormatGemini, `{"contents":[{"role":"model","parts":[{"text":"banned"},{"functionCall":{"name":"run","args":{"cmd":"x"}}}]}]}`, extracted{assistant: "banned cmd x"}},
	})
}

func TestExtractModerationInputTurnsAndUserMarks(t *testing.T) {
	input := ExtractModerationInput(sdktranslator.FormatClaude, []byte(`{
		"system":"sys",
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"},{"type":"text","text":"second"}]}
		]}`))
	want := []Segment{
		{Class: ClassPrimary, User: false, Turn: 0, Text: "sys"},
		{Class: ClassPrimary, User: true, Turn: 1, Text: "first"},
		{Class: ClassAssistantHistory, User: false, Turn: 2, Text: "reply"},
		{Class: ClassToolOutput, User: false, Turn: 3, Text: "out"},
		{Class: ClassPrimary, User: true, Turn: 3, Text: "second"},
	}
	if len(input.Segments) != len(want) {
		t.Fatalf("segments = %+v", input.Segments)
	}
	for i := range want {
		if input.Segments[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v", i, input.Segments[i], want[i])
		}
	}
}

// Whitespace inside a fragment collapses, as the single-string extractor did,
// so a keyword with one space matches across line breaks.
func TestExtractModerationInputCollapsesWhitespace(t *testing.T) {
	got := extractByClass(sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"  bad\n\n\tword  "}]}`)
	if got.primary != "bad word" {
		t.Fatalf("primary = %q", got.primary)
	}
}
