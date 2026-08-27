package executor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestMaybeEnsureCodexImageGenerationToolInjectsWhenEnabled(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"codex_image_generation_bridge": true,
		},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if got := gjson.GetBytes(out, "tools.#").Int(); got != 2 {
		t.Fatalf("tools count = %d, want 2; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "image_generation" {
		t.Fatalf("tools.1.type = %q, want image_generation; body=%s", got, out)
	}
}

func TestMaybeEnsureCodexImageGenerationToolSkipsWhenDisabled(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"codex_image_generation_bridge": false,
		},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if got := gjson.GetBytes(out, "tools.#").Int(); got != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", got, out)
	}
}

func TestMaybeEnsureCodexImageGenerationToolSkipsExistingImageGenNamespace(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"codex_image_generation_bridge": true,
		},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if got := gjson.GetBytes(out, "tools.#").Int(); got != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", got, out)
	}
}

func TestMaybeEnsureCodexImageGenerationToolSkipsSparkAndLite(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"codex_image_generation_bridge": true,
		},
	}
	body := []byte(`{"model":"gpt-5.3-codex-spark"}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.3-codex-spark", nil)
	if gjson.GetBytes(out, "tools").Exists() {
		t.Fatalf("spark model should not inject tools; body=%s", out)
	}

	liteBody := []byte(`{"model":"gpt-5.4"}`)
	headers := http.Header{}
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	out = maybeEnsureCodexImageGenerationTool(liteBody, auth, "gpt-5.4", headers)
	if gjson.GetBytes(out, "tools").Exists() {
		t.Fatalf("responses-lite should not inject tools; body=%s", out)
	}
}

func TestNormalizeCodexImageGenerationCallStatusCompletesGeneratingWithResult(t *testing.T) {
	in := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"generating","result":"iVBORw0KGgo="}}`)
	out := normalizeCodexImageGenerationCallStatus(in)
	if got := gjson.GetBytes(bytes.TrimSpace(out[5:]), "item.status").String(); got != "completed" {
		t.Fatalf("item.status = %q, want completed; out=%s", got, out)
	}
	if !bytes.HasPrefix(out, dataTag) {
		t.Fatalf("expected SSE data prefix, got %s", out)
	}
}

func TestNormalizeCodexImageGenerationCallStatusCompletesResponseOutput(t *testing.T) {
	in := []byte(`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"generating","result":"ZmFrZQ=="},{"type":"message","status":"completed"}]}}`)
	out := normalizeCodexImageGenerationCallStatus(in)
	if got := gjson.GetBytes(out, "response.output.0.status").String(); got != "completed" {
		t.Fatalf("output.0.status = %q, want completed; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "response.output.1.status").String(); got != "completed" {
		t.Fatalf("output.1.status = %q, want completed; out=%s", got, out)
	}
}

func TestNormalizeCodexImageGenerationCallStatusSkipsWithoutResult(t *testing.T) {
	in := []byte(`{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"generating"}}`)
	out := normalizeCodexImageGenerationCallStatus(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("payload changed without result: %s", out)
	}
}

func TestMaybeEnsureStripsHostedWhenLocalImageGenPresent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"codex_image_generation_bridge": true},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"},{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}],"tool_choice":{"type":"image_generation"}}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if gjson.GetBytes(out, "tools.#").Int() != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", gjson.GetBytes(out, "tools.#").Int(), out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "image_gen" {
		t.Fatalf("remaining tool = %q, want image_gen; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", got, out)
	}
}

func TestMaybeEnsureInjectsHostedWhenNoLocalImageGenEvenForDesktop(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"codex_image_generation_bridge": true},
	}
	headers := http.Header{}
	headers.Set("Originator", "Codex Desktop")
	headers.Set("User-Agent", "Codex Desktop/0.144.2")
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}],"instructions":"base"}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", headers)
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "image_generation" {
		t.Fatalf("tools.1.type = %q, want image_generation; body=%s", got, out)
	}
	if !strings.Contains(gjson.GetBytes(out, "instructions").String(), "cliproxy-codex-image-generation") {
		t.Fatalf("instructions missing bridge text; body=%s", out)
	}
}

func TestSynthesizeCodexImageDisplayMessageEvent(t *testing.T) {
	in := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png","revised_prompt":"cute cat"}}`)
	events := normalizeCodexImageGenerationOutboundEvent(in)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	display := bytes.TrimSpace(events[1][len(dataTag):])
	if got := gjson.GetBytes(display, "item.type").String(); got != "message" {
		t.Fatalf("display item.type = %q, want message; %s", got, display)
	}
	if got := gjson.GetBytes(display, "item.role").String(); got != "assistant" {
		t.Fatalf("display role = %q, want assistant", got)
	}
	text := gjson.GetBytes(display, "item.content.0.text").String()
	if !strings.Contains(text, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("display text missing data url: %s", text)
	}
}

func TestCodexImageStreamRewritesMntDataMarkdownAfterHostedImage(t *testing.T) {
	n := newCodexImageStreamNormalizer()
	ig := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"}}`)
	events := normalizeCodexImageGenerationOutboundEventWithState(n, ig)
	if len(events) < 1 {
		t.Fatalf("expected image events, got %d", len(events))
	}
	if len(n.images) != 1 {
		t.Fatalf("cached images = %d, want 1", len(n.images))
	}

	// Desktop-visible path: model writes ChatGPT sandbox markdown.
	textDone := []byte(`data: {"type":"response.output_text.done","item_id":"msg_1","text":"给你画好了：\n\n![小猫](/mnt/data/0.png)\n"}`)
	rewritten := normalizeCodexImageGenerationOutboundEventWithState(n, textDone)
	if len(rewritten) != 1 {
		t.Fatalf("text events = %d, want 1", len(rewritten))
	}
	body := bytes.TrimSpace(rewritten[0][len(dataTag):])
	got := gjson.GetBytes(body, "text").String()
	if !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("text not rewritten to data url: %s", got)
	}
	if strings.Contains(got, "/mnt/data/") {
		t.Fatalf("text still has /mnt/data: %s", got)
	}
	if !strings.Contains(got, "![小猫](") {
		t.Fatalf("alt text lost: %s", got)
	}

	msgDone := []byte(`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"给你画好了：\n\n![小猫](/mnt/data/0.png)\n"}]}}`)
	rewritten = normalizeCodexImageGenerationOutboundEventWithState(n, msgDone)
	body = bytes.TrimSpace(rewritten[0][len(dataTag):])
	got = gjson.GetBytes(body, "item.content.0.text").String()
	if !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo=") || strings.Contains(got, "/mnt/data/") {
		t.Fatalf("message item not rewritten: %s", got)
	}
}

func TestCodexImageStreamDoesNotRewriteWithoutHostedImage(t *testing.T) {
	n := newCodexImageStreamNormalizer()
	in := []byte(`data: {"type":"response.output_text.done","text":"see ![x](/mnt/data/0.png)"}`)
	out := normalizeCodexImageGenerationOutboundEventWithState(n, in)
	body := bytes.TrimSpace(out[0][len(dataTag):])
	if got := gjson.GetBytes(body, "text").String(); got != "see ![x](/mnt/data/0.png)" {
		t.Fatalf("rewrote without image result: %s", got)
	}
}

func TestRewriteCodexMntDataImageMarkdownClampsIndex(t *testing.T) {
	images := []codexHostedImage{{Result: "AAA", MIME: "image/png"}}
	got, ok := rewriteCodexMntDataImageMarkdown("![a](/mnt/data/3.png)", images)
	if !ok || !strings.Contains(got, "data:image/png;base64,AAA") {
		t.Fatalf("clamp rewrite failed: ok=%v got=%s", ok, got)
	}
}

func TestCodexImageStreamAppendsDataURLWhenModelOmitsMntPath(t *testing.T) {
	// Real Desktop failure mode after #699: model writes plain text "给你一只小猫。" with no /mnt/data.
	n := newCodexImageStreamNormalizer()
	ig := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"}}`)
	_ = normalizeCodexImageGenerationOutboundEventWithState(n, ig)

	textDone := []byte(`data: {"type":"response.output_text.done","item_id":"msg_1","text":"给你一只小猫。\n\n如果你要，我还能继续改。"}`)
	out := normalizeCodexImageGenerationOutboundEventWithState(n, textDone)
	body := bytes.TrimSpace(out[0][len(dataTag):])
	got := gjson.GetBytes(body, "text").String()
	if !strings.Contains(got, "给你一只小猫。") {
		t.Fatalf("original text lost: %s", got)
	}
	if !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("expected appended data url: %s", got)
	}

	msgDone := []byte(`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"给你一只小猫。"}]}}`)
	out = normalizeCodexImageGenerationOutboundEventWithState(n, msgDone)
	body = bytes.TrimSpace(out[0][len(dataTag):])
	got = gjson.GetBytes(body, "item.content.0.text").String()
	if !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("message item missing appended image: %s", got)
	}
}

func TestCodexImageStreamDoesNotDoubleAppendWhenDataURLPresent(t *testing.T) {
	n := newCodexImageStreamNormalizer()
	ig := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"}}`)
	_ = normalizeCodexImageGenerationOutboundEventWithState(n, ig)
	in := []byte(`data: {"type":"response.output_text.done","text":"done\n\n![x](data:image/png;base64,iVBORw0KGgo=)"}`)
	out := normalizeCodexImageGenerationOutboundEventWithState(n, in)
	body := bytes.TrimSpace(out[0][len(dataTag):])
	got := gjson.GetBytes(body, "text").String()
	if strings.Count(got, "data:image/png;base64,iVBORw0KGgo=") != 1 {
		t.Fatalf("double-appended image: %s", got)
	}
}

func TestCodexImageStreamRewritesLocalGeneratedImagesPathWithoutDouble(t *testing.T) {
	// Real Desktop bug: model invents ~/.codex/generated_images/... then we used to APPEND a second data-url.
	n := newCodexImageStreamNormalizer()
	ig := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"}}`)
	_ = normalizeCodexImageGenerationOutboundEventWithState(n, ig)
	text := "给你画好了：\n\n![可爱的小鱼](/Users/kittors/.codex/generated_images/abc.png)\n\n如果你要，我还能继续改。"
	in := []byte(`data: {"type":"response.output_text.done","text":` + mustJSONString(text) + `}`)
	out := normalizeCodexImageGenerationOutboundEventWithState(n, in)
	body := bytes.TrimSpace(out[0][len(dataTag):])
	got := gjson.GetBytes(body, "text").String()
	if strings.Contains(got, "generated_images/") {
		t.Fatalf("local path should be rewritten: %s", got)
	}
	if strings.Count(got, "data:image/png;base64,iVBORw0KGgo=") != 1 {
		t.Fatalf("want exactly one data url, got %q", got)
	}
	if !strings.Contains(got, "![可爱的小鱼](data:image/png;base64,iVBORw0KGgo=)") {
		t.Fatalf("alt/path rewrite failed: %s", got)
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestMaybeEnsureForcesToolChoiceOnImageIntent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"codex_image_generation_bridge": true},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"[$imagegen] 给我画一个小猫"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "image_generation" {
		t.Fatalf("tools.1.type = %q, want image_generation; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; body=%s", got, out)
	}
}

func TestMaybeEnsureDoesNotForceToolChoiceWithoutIntent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"codex_image_generation_bridge": true},
	}
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor this function"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.4", nil)
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "image_generation" {
		t.Fatalf("tools.1.type = %q, want image_generation; body=%s", got, out)
	}
	if gjson.GetBytes(out, "tool_choice.type").String() == "image_generation" {
		t.Fatalf("tool_choice should not force image_generation without intent; body=%s", out)
	}
}

func TestMaybeEnsureInjectsWhenBridgeMetadataAbsent(t *testing.T) {
	// A fresh Codex OAuth account carries no bridge metadata. Requiring an explicit opt-in
	// left the Image Generation skill dead for every account nobody had toggled by hand.
	for name, auth := range map[string]*cliproxyauth.Auth{
		"nil metadata":   {Provider: "codex"},
		"empty metadata": {Provider: "codex", Metadata: map[string]any{}},
		"other metadata": {Provider: "codex", Metadata: map[string]any{"email": "a@b.c"}},
	} {
		body := []byte(`{"model":"gpt-5.6-luna","tools":[{"type":"function","name":"shell"}]}`)
		out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", nil)
		if got := gjson.GetBytes(out, "tools.1.type").String(); got != "image_generation" {
			t.Fatalf("%s: tools.1.type = %q, want image_generation; body=%s", name, got, out)
		}
	}
}

func TestMaybeEnsureSkipsWhenBridgeExplicitlyDisabled(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"codex_image_generation_bridge": false},
	}
	body := []byte(`{"model":"gpt-5.6-luna","tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我画一个小猫咪"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", nil)
	if gjson.GetBytes(out, "tools.#").Int() != 1 {
		t.Fatalf("opt-out account received the hosted tool; body=%s", out)
	}
}

func TestMaybeEnsureInjectsToolShapeThatUpstreamInvokes(t *testing.T) {
	// action + model mirror buildCodexImageResponsesRequest, the request shape verified to
	// return images from chatgpt.com/backend-api/codex/responses. A bare
	// {"type":"image_generation"} is accepted upstream but never invoked by the model.
	auth := &cliproxyauth.Auth{Provider: "codex"}
	body := []byte(`{"model":"gpt-5.6-luna","tools":[{"type":"function","name":"shell"}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", nil)
	tool := gjson.GetBytes(out, "tools.1")
	if got := tool.Get("action").String(); got != "generate" {
		t.Fatalf("tool.action = %q, want generate; body=%s", got, out)
	}
	if got := tool.Get("model").String(); got != codexImageModel {
		t.Fatalf("tool.model = %q, want %q; body=%s", got, codexImageModel, out)
	}
}

func TestMaybeEnsureUsesEditActionWhenTurnCarriesImage(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex"}
	body := []byte(`{"model":"gpt-5.6-luna","tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"把这张图改成蓝色"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", nil)
	if got := gjson.GetBytes(out, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tool.action = %q, want edit; body=%s", got, out)
	}
}

func TestMaybeEnsureKeepsGenerateActionForTextOnlyTurn(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex"}
	body := []byte(`{"model":"gpt-5.6-luna","tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我画一个小猫咪"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", nil)
	if got := gjson.GetBytes(out, "tools.0.action").String(); got != "generate" {
		t.Fatalf("tool.action = %q, want generate; body=%s", got, out)
	}
}

func TestMaybeEnsureInjectsForLiteRequestWithImageIntent(t *testing.T) {
	// Lite normally keeps its tools in input[].additional_tools, so the hosted tool is only
	// worth the unverified upstream shape on turns that actually ask for an image.
	auth := &cliproxyauth.Auth{Provider: "codex"}
	headers := http.Header{}
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	body := []byte(`{"model":"gpt-5.6-luna","tools":[],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"[$imagegen](/Users/x/.codex/skills/.system/imagegen/SKILL.md) 帮我画一个小猫咪"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.6-luna", headers)
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; body=%s", got, out)
	}
}

func TestRequestLooksLikeImageGenerationIntentMatchesDesktopSkillPayload(t *testing.T) {
	// Verbatim shapes captured from a Codex Desktop turn that had the Image Gen skill on.
	for name, text := range map[string]string{
		"skill link":  "[$imagegen](/Users/kittors/.codex/skills/.system/imagegen/SKILL.md)  帮我画一个小猫咪\n",
		"skill block": "<skill>\n<name>imagegen</name>\n<path>/Users/kittors/.codex/skills/.system/imagegen/SKILL.md</path>\n",
		"plain ask":   "帮我画一个小猫咪\n",
	} {
		body, err := json.Marshal(map[string]any{
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": text}},
			}},
		})
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if !requestLooksLikeImageGenerationIntent(body) {
			t.Fatalf("%s: intent not detected for %q", name, text)
		}
	}
}

func TestStripCodexHistoryDataURLImagesReplacesHugeMarkdown(t *testing.T) {
	// ~2KB base64 payload (still above 256 threshold); real Desktop history is multi-MB.
	b64 := strings.Repeat("A", 400)
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"画小猫"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"给你画好了。\n\n![generated image](data:image/png;base64,` + b64 + `)"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"长什么样？"}]}
		]
	}`)
	out := stripCodexHistoryDataURLImages(body)
	asst := gjson.GetBytes(out, "input.1.content.0.text").String()
	if strings.Contains(asst, "base64,") || strings.Contains(asst, strings.Repeat("A", 50)) {
		preview := asst
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("base64 should be stripped; text_len=%d text=%q", len(asst), preview)
	}
	if !strings.Contains(asst, "![generated image](cliproxy-image:1)") {
		t.Fatalf("want placeholder, got %q", asst)
	}
	if !strings.Contains(asst, "给你画好了") {
		t.Fatalf("caption lost: %q", asst)
	}
	// Follow-up user text untouched.
	if got := gjson.GetBytes(out, "input.2.content.0.text").String(); got != "长什么样？" {
		t.Fatalf("user follow-up changed: %q", got)
	}
}

func TestStripCodexHistoryDataURLImagesLeavesSmallDataURL(t *testing.T) {
	// Tiny icons / test fixtures must not become placeholders.
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"see ![x](data:image/png;base64,aGVsbG8=)"}]}]}`)
	out := stripCodexHistoryDataURLImages(body)
	got := gjson.GetBytes(out, "input.0.content.0.text").String()
	if !strings.Contains(got, "data:image/png;base64,aGVsbG8=") {
		t.Fatalf("small data url should be kept: %q", got)
	}
}

func TestSanitizeCodexResponsesRequestStripsHistoryDataURL(t *testing.T) {
	b64 := strings.Repeat("B", 400)
	body := []byte(`{"model":"gpt-5.4","max_tokens":99,"input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"![cat](data:image/png;base64,` + b64 + `)"}]}]}`)
	out := sanitizeCodexResponsesRequest(body)
	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should still be stripped")
	}
	text := gjson.GetBytes(out, "input.0.content.0.text").String()
	if strings.Contains(text, "base64,") {
		t.Fatalf("history data url survived sanitize: %q", text)
	}
	if !strings.Contains(text, "cliproxy-image:") {
		t.Fatalf("missing placeholder: %q", text)
	}
}

func TestStripCodexHistoryDataURLImagesDropsImageGenerationCallResult(t *testing.T) {
	b64 := strings.Repeat("C", 400)
	body := []byte(`{"store":false,"input":[{"type":"image_generation_call","id":"ig_1","result":"` + b64 + `","status":"completed"}]}`)
	out := stripCodexHistoryDataURLImages(body)
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("result").Exists() {
			t.Fatalf("image_generation_call.result should be deleted; payload=%s", out)
		}
	}
	if bytes.Contains(out, []byte(`"ig_1"`)) {
		t.Fatalf("stored image_generation_call id should not be forwarded when store=false; payload=%s", out)
	}
	if got := gjson.GetBytes(out, "input.#").Int(); got != 0 {
		t.Fatalf("image_generation_call without result should be dropped, got %d items; payload=%s", got, out)
	}
}

func TestStripCodexHistoryDataURLImagesDropsImageGenerationCallReferenceOnlyItem(t *testing.T) {
	body := []byte(`{"store":false,"input":[{"type":"image_generation_call","id":"ig_reference_only","status":"completed"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	out := stripCodexHistoryDataURLImages(body)
	if bytes.Contains(out, []byte(`"ig_reference_only"`)) {
		t.Fatalf("reference-only image_generation_call should not be forwarded when store=false; payload=%s", out)
	}
	if got := gjson.GetBytes(out, "input.#").Int(); got != 1 {
		t.Fatalf("input item count = %d, want 1; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "continue" {
		t.Fatalf("remaining user message changed: %q; payload=%s", got, out)
	}
}

func TestStripCodexHistoryDataURLImagesReattachesLastAsInputImage(t *testing.T) {
	// Follow-up "描述一下" must still see pixels via structured input_image, not text base64.
	b64 := strings.Repeat("D", 400)
	body := []byte(`{
		"store":false,
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"画小鱼"}]},
			{"type":"image_generation_call","id":"ig_previous","result":"` + b64 + `","status":"completed","output_format":"png"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"好了\n\n![generated image](data:image/png;base64,` + b64 + `)"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"你画的是啥，详细描述"}]}
		]
	}`)
	out := stripCodexHistoryDataURLImages(body)
	if bytes.Contains(out, []byte(`"ig_previous"`)) {
		t.Fatalf("stored image generation history should be removed; payload=%s", out)
	}
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("result").Exists() {
			t.Fatalf("stored image generation result should be removed; payload=%s", out)
		}
	}
	asst := gjson.GetBytes(out, "input.1.content.0.text").String()
	if strings.Contains(asst, "base64,") {
		t.Fatalf("assistant text still has base64")
	}
	if !strings.Contains(asst, "cliproxy-image:1") {
		t.Fatalf("placeholder missing: %q", asst)
	}
	// Latest user turn should gain input_image with the extracted bytes.
	parts := gjson.GetBytes(out, "input.2.content")
	if !parts.IsArray() || parts.Get("#").Int() < 2 {
		t.Fatalf("user content should be multipart with image; %s", parts.Raw)
	}
	found := false
	for _, part := range parts.Array() {
		if part.Get("type").String() != "input_image" {
			continue
		}
		url := part.Get("image_url").String()
		if strings.Contains(url, "base64,"+b64) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected input_image with history bytes; content=%s", parts.Raw)
	}
	// User text preserved.
	if got := gjson.GetBytes(out, "input.2.content.0.text").String(); got != "你画的是啥，详细描述" {
		t.Fatalf("user text lost: %q", got)
	}
}

func TestStripCodexHistoryDoesNotDuplicateInputImage(t *testing.T) {
	b64 := strings.Repeat("E", 400)
	body := []byte(`{
		"input":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"![x](data:image/png;base64,` + b64 + `)"}]},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"看这张"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}
			]}
		]
	}`)
	out := stripCodexHistoryDataURLImages(body)
	// Existing user input_image means we must not attach another.
	count := 0
	for _, part := range gjson.GetBytes(out, "input.1.content").Array() {
		if part.Get("type").String() == "input_image" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("input_image count = %d, want 1; %s", count, gjson.GetBytes(out, "input.1.content").Raw)
	}
}
