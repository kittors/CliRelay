package contentmoderation

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestExtractModeratableText(t *testing.T) {
	tests := []struct {
		name   string
		format sdktranslator.Format
		body   string
		want   string
	}{
		{"openai chat", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","content":"old"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`, "hello world"},
		{"openai image prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-image-2","prompt":"bad word"}`, "bad word"},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"old"}]},{"role":"user","content":[{"type":"input_text","text":"response text"}]}]}`, "response text"},
		{"claude", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"claude text"}]}]}`, "claude text"},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"gemini text"}]}]}`, "gemini text"},
		{"openai empty body", sdktranslator.FormatOpenAI, `{}`, ""},
		{"openai no prompt", sdktranslator.FormatOpenAI, `{"model":"gpt-image-2"}`, ""},

		// Every caller-controlled turn must be moderated, not only the newest one.
		{
			"openai all user turns",
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"generated"},{"role":"user","content":"second"}]}`,
			"first second",
		},
		{
			"openai trailing empty user does not hide earlier turn",
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"user","content":"banned"},{"role":"user","content":""}]}`,
			"banned",
		},
		{
			"openai trailing image-only user does not hide earlier turn",
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"user","content":"banned"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.invalid/a.png"}}]}]}`,
			"banned",
		},
		{
			"openai system role is moderated",
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"system","content":"banned"},{"role":"user","content":"hi"}]}`,
			"banned hi",
		},
		{
			"claude system field is moderated",
			sdktranslator.FormatClaude,
			`{"system":"banned","messages":[{"role":"user","content":"hi"}]}`,
			"banned hi",
		},
		{
			"claude system reminder is skipped",
			sdktranslator.FormatClaude,
			`{"messages":[{"role":"user","content":"real"},{"role":"user","content":"<system-reminder>noise</system-reminder>"}]}`,
			"real",
		},
		{
			"responses all user turns",
			sdktranslator.FormatOpenAIResponse,
			`{"input":[{"role":"user","content":[{"type":"input_text","text":"banned"}]},{"role":"assistant","content":[{"type":"output_text","text":"generated"}]},{"role":"user","content":[{"type":"input_text","text":""}]}]}`,
			"banned",
		},
		{
			"responses instructions are moderated",
			sdktranslator.FormatOpenAIResponse,
			`{"instructions":"banned","input":"hi"}`,
			"hi banned",
		},
		{
			"gemini all user turns",
			sdktranslator.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"text":"banned"}]},{"role":"model","parts":[{"text":"generated"}]},{"role":"user","parts":[{"text":""}]}]}`,
			"banned",
		},
		{
			"gemini system instruction is moderated",
			sdktranslator.FormatGemini,
			`{"systemInstruction":{"parts":[{"text":"banned"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			"banned hi",
		},
		{
			"assistant-only payload yields nothing",
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"assistant","content":"generated"}]}`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractModeratableText(tt.format, []byte(tt.body)); got != tt.want {
				t.Fatalf("ExtractModeratableText = %q, want %q", got, tt.want)
			}
		})
	}
}
