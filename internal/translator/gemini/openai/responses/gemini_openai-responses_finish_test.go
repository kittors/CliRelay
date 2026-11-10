package responses

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chunks in the shape Gemini streams them. The text is the one a real
// MALFORMED_FUNCTION_CALL turn left behind (#1144): Gemini wrote the tool call
// out as text, then gave up.
const (
	finishTestThought = `{"candidates":[{"content":{"role":"model","parts":[{"text":"I will edit the file.","thought":true}]}}],"responseId":"r1"}`
	finishTestText    = `{"candidates":[{"content":{"role":"model","parts":[{"text":"call:default_api:edit{edits:["}]}}],"responseId":"r1"}`
	finishTestCall    = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"bash","args":{"command":"ls"}}}]}}],"responseId":"r1"}`
)

// finishTestEnd is the chunk that ends the turn with reason.
func finishTestEnd(reason string) string {
	end := `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":""}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":6,"totalTokenCount":20},"responseId":"r1"}`
	end, _ = sjson.Set(end, "candidates.0.finishReason", reason)
	if reason == "MALFORMED_FUNCTION_CALL" {
		end, _ = sjson.Set(end, "candidates.0.finishMessage", "Malformed function call: Failed to parse function call: Function call is empty - no input to parse.")
	}
	return end
}

// finishTestNonStream is the same turn as one non-streaming response.
func finishTestNonStream(chunks []string, reason string) string {
	body := finishTestEnd(reason)
	body, _ = sjson.SetRaw(body, "candidates.0.content.parts", "[]")
	for _, chunk := range chunks {
		gjson.Get(chunk, "candidates.0.content.parts").ForEach(func(_, part gjson.Result) bool {
			body, _ = sjson.SetRaw(body, "candidates.0.content.parts.-1", part.Raw)
			return true
		})
	}
	return body
}

type geminiFinishCase struct {
	name   string
	chunks []string // what streams before the finishing chunk
	reason string
	// status is the Responses status, and the terminal event is "response."+status.
	status string
	// incompleteReason is incomplete_details.reason; "" means incomplete_details is null.
	incompleteReason string
	outputTypes      []string // the item types in the response output
}

var geminiFinishCases = []geminiFinishCase{
	{"stop after text", []string{finishTestThought, finishTestText}, "STOP", "completed", "", []string{"reasoning", "message"}},
	{"stop after a function call", []string{finishTestThought, finishTestCall}, "STOP", "completed", "", []string{"reasoning", "function_call"}},
	{"stop with nothing but thoughts", []string{finishTestThought}, "STOP", "completed", "", []string{"reasoning"}},
	{"unspecified", []string{finishTestText}, "FINISH_REASON_UNSPECIFIED", "completed", "", []string{"message"}},
	{"max tokens", []string{finishTestThought, finishTestText}, "MAX_TOKENS", "incomplete", "max_output_tokens", []string{"reasoning", "message"}},
	{"max tokens in lower case", []string{finishTestText}, "max_tokens", "incomplete", "max_output_tokens", []string{"message"}},
	{"max tokens while thinking", []string{finishTestThought}, "MAX_TOKENS", "incomplete", "max_output_tokens", []string{"reasoning"}},
	{"safety", []string{finishTestText}, "SAFETY", "incomplete", "content_filter", []string{"message"}},
	{"recitation", []string{finishTestText}, "RECITATION", "incomplete", "content_filter", []string{"message"}},
	{"prohibited content", []string{finishTestThought}, "PROHIBITED_CONTENT", "incomplete", "content_filter", []string{"reasoning"}},
	{"image prohibited content", []string{finishTestThought}, "IMAGE_PROHIBITED_CONTENT", "incomplete", "content_filter", []string{"reasoning"}},
	// A malformed call after visible output is a reply cut short, reported the
	// way a truncated reply is. Responses has no reason for it (see
	// geminiResponsesFinishOf), so incomplete_details is null.
	{"malformed call after text", []string{finishTestThought, finishTestText}, "MALFORMED_FUNCTION_CALL", "incomplete", "", []string{"reasoning", "message"}},
	{"malformed call after a function call", []string{finishTestThought, finishTestCall}, "MALFORMED_FUNCTION_CALL", "incomplete", "", []string{"reasoning", "function_call"}},
	// Reaches the translator once the Antigravity executor has spent its
	// retries, and always from Gemini and Gemini CLI, which do not retry.
	{"malformed call after thoughts only", []string{finishTestThought}, "MALFORMED_FUNCTION_CALL", "incomplete", "", []string{"reasoning"}},
	// Reasons with no settled mapping yet end the turn as before.
	{"other", []string{finishTestText}, "OTHER", "completed", "", []string{"message"}},
	{"unexpected tool call", []string{finishTestText}, "UNEXPECTED_TOOL_CALL", "completed", "", []string{"message"}},
}

// checkGeminiFinish checks status and incomplete_details on a Responses object.
func checkGeminiFinish(t *testing.T, resp gjson.Result, tc geminiFinishCase) {
	t.Helper()
	if got := resp.Get("status").String(); got != tc.status {
		t.Fatalf("status = %q, want %q: %s", got, tc.status, resp.Raw)
	}
	details := resp.Get("incomplete_details")
	if tc.status != "incomplete" {
		if details.Exists() && details.Type != gjson.Null {
			t.Fatalf("incomplete_details = %s on a %s response", details.Raw, tc.status)
		}
		return
	}
	if tc.incompleteReason == "" {
		// null, not {}: the Vercel AI SDK rejects an object without a reason.
		if !details.Exists() || details.Type != gjson.Null {
			t.Fatalf("incomplete_details = %s, want null: %s", details.Raw, resp.Raw)
		}
		return
	}
	if !details.IsObject() || details.Get("reason").String() != tc.incompleteReason {
		t.Fatalf("incomplete_details = %s, want reason %q", details.Raw, tc.incompleteReason)
	}
}

// checkGeminiFinishOutput checks that the output produced before the turn ended
// is still delivered. Order is not compared: the non-stream converter lists
// function calls before the reasoning and the message.
func checkGeminiFinishOutput(t *testing.T, resp gjson.Result, tc geminiFinishCase) {
	t.Helper()
	var types []string
	resp.Get("output").ForEach(func(_, item gjson.Result) bool {
		types = append(types, item.Get("type").String())
		return true
	})
	want := append([]string(nil), tc.outputTypes...)
	sort.Strings(types)
	sort.Strings(want)
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("output types = %q, want %q: %s", types, want, resp.Get("output").Raw)
	}
	// What was produced before the turn ended still reaches the client.
	if got := resp.Get("usage.output_tokens").Int(); got != 4 {
		t.Fatalf("usage.output_tokens = %d, want 4", got)
	}
}

func TestGeminiResponsesStreamEndsAsTheFinishReasonSays(t *testing.T) {
	for _, tc := range geminiFinishCases {
		t.Run(tc.name, func(t *testing.T) {
			var param any
			var out []string
			chunks := append(append([]string{}, tc.chunks...), finishTestEnd(tc.reason), "[DONE]")
			for _, chunk := range chunks {
				out = append(out, ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-test", nil, nil, []byte(chunk), &param)...)
			}
			if len(out) == 0 {
				t.Fatal("no events")
			}
			terminals := 0
			for _, chunk := range out {
				switch event, _ := parseSSEEvent(t, chunk); event {
				case "response.completed", "response.incomplete", "response.failed":
					terminals++
				}
			}
			event, data := parseSSEEvent(t, out[len(out)-1])
			want := "response." + tc.status
			if terminals != 1 || event != want {
				t.Fatalf("last event = %q with %d terminal events, want one %s at the end", event, terminals, want)
			}
			if got := data.Get("type").String(); got != want {
				t.Fatalf("terminal event type = %q, want %q", got, want)
			}
			checkGeminiFinish(t, data.Get("response"), tc)
			checkGeminiFinishOutput(t, data.Get("response"), tc)
		})
	}
}

func TestGeminiResponsesNonStreamStatusFollowsTheFinishReason(t *testing.T) {
	for _, tc := range geminiFinishCases {
		t.Run(tc.name, func(t *testing.T) {
			body := finishTestNonStream(tc.chunks, tc.reason)
			resp := gjson.Parse(ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-test", nil, nil, []byte(body), nil))
			checkGeminiFinish(t, resp, tc)
			checkGeminiFinishOutput(t, resp, tc)
		})
	}
}
