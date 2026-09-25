package translator

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	"github.com/tidwall/gjson"
)

// This matrix sends one screenshot tool result through every registered
// request translator that reads tool results, in every image shape the client
// protocol allows, and checks that the image leaves as the target's image type
// rather than as base64 text.

var matrixImageData = strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 256) // ~5.6 KB of base64

const matrixImageURL = "https://example.com/shot.png"

// imageShape is one way a client protocol writes an image in a tool result.
type imageShape struct {
	name   string
	part   string // JSON of the image part
	remote bool   // true when the image is a URL, not inline data
}

var (
	responsesShapes = []imageShape{
		{"input_image data url string", `{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,` + matrixImageData + `"}`, false},
		{"input_image data url object", `{"type":"input_image","image_url":{"url":"data:image/png;base64,` + matrixImageData + `"}}`, false},
		{"input_image remote url", `{"type":"input_image","image_url":"` + matrixImageURL + `"}`, true},
	}
	chatShapes = []imageShape{
		{"image_url data url", `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + matrixImageData + `","detail":"auto"}}`, false},
		{"image_url remote url", `{"type":"image_url","image_url":{"url":"` + matrixImageURL + `"}}`, true},
	}
	claudeShapes = []imageShape{
		{"image base64", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + matrixImageData + `"}}`, false},
		{"image url", `{"type":"image","source":{"type":"url","url":"` + matrixImageURL + `"}}`, true},
	}
)

// Requests carrying the tool result, one builder per client protocol. The
// tool result is always text followed by the image part.
func responsesRequest(image string) string {
	return `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"take a screenshot"}]},
		{"type":"function_call","call_id":"call_1","name":"shot","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"Took a screenshot."},` + image + `]}]}`
}

func chatRequest(image string) string {
	return `{"model":"m","messages":[
		{"role":"user","content":"take a screenshot"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"Took a screenshot."},` + image + `]}]}`
}

func claudeRequest(image string) string {
	return `{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"take a screenshot"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"Took a screenshot."},` + image + `]}]}]}`
}

// Checks, one per target protocol. Each finds the tool result in the
// translated request and asserts the image is an image part there.
type imageCheck func(t *testing.T, out []byte, remote bool)

func checkClaude(t *testing.T, out []byte, remote bool) {
	content := gjson.GetBytes(out, `messages.#.content.#(type=="tool_result")|0.content`)
	img := content.Get(`#(type=="image")`)
	if !img.Exists() {
		t.Fatalf("no image block in tool_result content: %.300s", content.Raw)
	}
	if remote {
		assertEqual(t, "source.url", img.Get("source.url").String(), matrixImageURL)
		return
	}
	assertEqual(t, "source.type", img.Get("source.type").String(), "base64")
	assertEqual(t, "source.media_type", img.Get("source.media_type").String(), "image/png")
	assertEqual(t, "source.data", img.Get("source.data").String(), matrixImageData)
}

func checkChat(t *testing.T, out []byte, remote bool) {
	content := gjson.GetBytes(out, `messages.#(role=="tool").content`)
	url := content.Get(`#(type=="image_url").image_url.url`).String()
	want := matrixImageURL
	if !remote {
		want = "data:image/png;base64," + matrixImageData
	}
	assertEqual(t, "image_url.url", url, want)
}

func checkResponses(t *testing.T, out []byte, remote bool) {
	output := gjson.GetBytes(out, `input.#(type=="function_call_output").output`)
	url := output.Get(`#(type=="input_image").image_url`).String()
	want := matrixImageURL
	if !remote {
		want = "data:image/png;base64," + matrixImageData
	}
	assertEqual(t, "input_image.image_url", url, want)
}

func checkGemini(t *testing.T, out []byte, remote bool) {
	root := gjson.ParseBytes(out)
	if req := root.Get("request"); req.Exists() {
		root = req // gemini-cli and antigravity wrap the Gemini body
	}
	fr := root.Get(`contents.#.parts.#(functionResponse)|0.functionResponse`)
	if !fr.Exists() {
		t.Fatalf("no functionResponse: %.300s", out)
	}
	if remote {
		assertEqual(t, "parts.fileData.fileUri", fr.Get("parts.0.fileData.fileUri").String(), matrixImageURL)
	} else {
		assertEqual(t, "parts.inlineData.mimeType", fr.Get("parts.0.inlineData.mimeType").String(), "image/png")
		assertEqual(t, "parts.inlineData.data", fr.Get("parts.0.inlineData.data").String(), matrixImageData)
	}
	assertEqual(t, "response.result", fr.Get("response.result").String(), "Took a screenshot.")
}

// checkAntigravityClaude asserts the base64 image reaches
// functionResponse.parts and no base64 remains in response.result.
func checkAntigravityClaude(t *testing.T, out []byte, _ bool) {
	fr := gjson.GetBytes(out, `request.contents.#.parts.#(functionResponse)|0.functionResponse`)
	assertEqual(t, "parts.inlineData.data", fr.Get("parts.0.inlineData.data").String(), matrixImageData)
	if strings.Contains(fr.Get("response").Raw, matrixImageData) {
		t.Fatal("image payload leaked into response.result")
	}
}

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %.120q, want %.120q", field, got, want)
	}
}

func TestToolResultImagesSurviveEveryTranslator(t *testing.T) {
	type path struct {
		from, to string
		build    func(string) string
		shapes   []imageShape
		check    imageCheck
	}
	paths := []path{
		{"openai-response", "claude", responsesRequest, responsesShapes, checkClaude},
		{"openai-response", "openai", responsesRequest, responsesShapes, checkChat},
		{"openai-response", "gemini", responsesRequest, responsesShapes, checkGemini},
		{"openai-response", "gemini-cli", responsesRequest, responsesShapes, checkGemini},
		{"openai-response", "antigravity", responsesRequest, responsesShapes, checkGemini},
		{"openai", "claude", chatRequest, chatShapes, checkClaude},
		{"openai", "codex", chatRequest, chatShapes, checkResponses},
		{"openai", "gemini", chatRequest, chatShapes, checkGemini},
		{"openai", "gemini-cli", chatRequest, chatShapes, checkGemini},
		{"openai", "antigravity", chatRequest, chatShapes, checkGemini},
		{"claude", "codex", claudeRequest, claudeShapes, checkResponses},
		{"claude", "openai", claudeRequest, claudeShapes, checkChat},
		{"claude", "gemini", claudeRequest, claudeShapes, checkGemini},
		{"claude", "gemini-cli", claudeRequest, claudeShapes, checkGemini},
		// Antigravity keeps its own tool_result conversion: base64 images go to
		// functionResponse.parts and text parts stay as their JSON objects in
		// response.result. URL images are not converted there (they carry no
		// base64, so they cannot inflate the token count).
		{"claude", "antigravity", claudeRequest, claudeShapes[:1], checkAntigravityClaude},
	}
	for _, p := range paths {
		for _, shape := range p.shapes {
			t.Run(p.from+"->"+p.to+"/"+shape.name, func(t *testing.T) {
				out := translator.Request(p.from, p.to, "m", []byte(p.build(shape.part)), false)
				if !gjson.ValidBytes(out) {
					t.Fatalf("translated request is not valid JSON: %.300s", out)
				}
				p.check(t, out, shape.remote)
			})
		}
	}
}

// A tool result without images must not change shape: plain strings stay
// strings on every path.
func TestToolResultPlainStringUnchanged(t *testing.T) {
	cases := []struct {
		from, to, req, path string
	}{
		{"openai-response", "claude", `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"plain"}]}`, `messages.#.content.#(type=="tool_result")|0.content`},
		{"openai-response", "openai", `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"plain"}]}`, `messages.#(role=="tool").content`},
		{"openai", "claude", `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"plain"}]}`, `messages.#.content.#(type=="tool_result")|0.content`},
		{"openai", "codex", `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"plain"}]}`, `input.#(type=="function_call_output").output`},
		{"claude", "codex", `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"plain"}]}]}`, `input.#(type=="function_call_output").output`},
		{"claude", "openai", `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"plain"}]}]}`, `messages.#(role=="tool").content`},
	}
	for _, c := range cases {
		t.Run(c.from+"->"+c.to, func(t *testing.T) {
			got := gjson.GetBytes(translator.Request(c.from, c.to, "m", []byte(c.req), false), c.path)
			if got.Type != gjson.String || got.String() != "plain" {
				t.Fatalf("content = %s, want the string \"plain\"", got.Raw)
			}
		})
	}
}
