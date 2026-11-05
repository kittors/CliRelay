package executor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Synthetic copies of the context blocks Codex 0.166 writes into a conversation. The skills
// list is the one that misfired in production: a third-party skill's description names the
// imagegen system skill, and the list is re-sent at the start of later turns.
const (
	testCodexSkillsInstructions = "<skills_instructions>\n## Skills\nA skill is a set of local instructions to follow that is stored in a `SKILL.md` file.\n### Available skills\n" +
		"- hatch-pet: Create an animated pet sprite atlas. This skill composes the installed $imagegen system skill for visual generation and uses bundled scripts for packaging.\n" +
		"- imagegen: Generate or edit raster images (image generation, 生图/出图). (file: /Users/x/.codex/skills/.system/imagegen/SKILL.md)\n" +
		"</skills_instructions>"
	testCodexAgentsMd      = "# AGENTS.md instructions for /Users/x/生图工具\n\n<INSTRUCTIONS>\nUI 改动先出图给我确认；不要用 image generation 代替设计稿。Use $imagegen only when asked.\n</INSTRUCTIONS>"
	testCodexEnvContext    = "<environment_context>\n  <cwd>/Users/x/draw a cat</cwd>\n  <shell>zsh</shell>\n</environment_context>"
	testCodexImagegenSkill = "<skill>\n<name>imagegen</name>\n<path>/Users/x/.codex/skills/.system/imagegen/SKILL.md</path>\n---\nname: \"imagegen\"\n---\n- **Default built-in tool mode (preferred):** built-in `image_gen` tool.\n</skill>"
	testCodexImageTag      = "<image name=[Image #1] path=/Users/x/Desktop/生成一张截图.png>"
)

func testCodexMessage(role string, texts ...string) map[string]any {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	parts := make([]any, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, map[string]any{"type": partType, "text": text})
	}
	return map[string]any{"type": "message", "role": role, "content": parts}
}

func testCodexToolRound(callID string) []any {
	return []any{
		map[string]any{"type": "function_call", "call_id": callID, "name": "exec_command", "arguments": `{"cmd":"rg -n wall src"}`},
		map[string]any{"type": "function_call_output", "call_id": callID, "output": "src/ui/WallButtons.tsx:12"},
	}
}

// testCodexChatBody is a /responses body as the chat-completions translator builds it from a
// Codex conversation relayed through a Chat hop: empty instructions, Codex function tools,
// the effort the client picked (none when effort is empty).
func testCodexChatBody(t *testing.T, effort string, input ...any) []byte {
	t.Helper()
	reasoning := map[string]any{"summary": "detailed"}
	if effort != "" {
		reasoning["effort"] = effort
	}
	body, err := json.Marshal(map[string]any{
		"model":        "gpt-6.1-sol",
		"instructions": "",
		"stream":       true,
		"reasoning":    reasoning,
		"tools":        []any{map[string]any{"type": "function", "name": "exec_command"}},
		"input":        input,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// testCodexPriorHistory is a session that opened with Codex's context, asked one coding
// question and ran a few tool rounds; its opening developer messages came through as
// developer, which is how the earlier turns of the failing session stayed healthy.
func testCodexPriorHistory() []any {
	history := []any{
		testCodexMessage("developer", "<permissions instructions>\nfull access\n</permissions instructions>", testCodexSkillsInstructions),
		testCodexMessage("user", testCodexAgentsMd),
		testCodexMessage("user", testCodexEnvContext),
		testCodexMessage("user", "梳理一下把两个墙体按钮合并成一个切换按钮的需求"),
		testCodexMessage("assistant", "我先看一下按钮实现。"),
	}
	history = append(history, testCodexToolRound("call_1")...)
	history = append(history, testCodexToolRound("call_2")...)
	return append(history, testCodexMessage("assistant", "需求梳理如下：合并为单个切换按钮。"))
}

func testCodexAuth() *cliproxyauth.Auth { return &cliproxyauth.Auth{Provider: "codex"} }

func assertCodexToolChoiceNotForced(t *testing.T, name string, out []byte) {
	t.Helper()
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got == "image_generation" {
		t.Fatalf("%s: tool_choice forced to image_generation; body=%s", name, out)
	}
}

// assertCodexImageCallWithoutEffort checks a request that is going to call the hosted tool:
// the tool is there, no reasoning.effort is left for the backend to turn into complexity, the
// rest of the reasoning object is untouched and no complexity field is invented.
func assertCodexImageCallWithoutEffort(t *testing.T, name string, out []byte) {
	t.Helper()
	if !requestHasHostedImageGenerationTool(out) {
		t.Fatalf("%s: hosted image tool missing; body=%s", name, out)
	}
	if gjson.GetBytes(out, "reasoning.effort").Exists() {
		t.Fatalf("%s: reasoning.effort still sent with an image call; body=%s", name, out)
	}
	if got := gjson.GetBytes(out, "reasoning.summary").String(); got != "detailed" {
		t.Fatalf("%s: reasoning.summary = %q, want the client's value kept; body=%s", name, got, out)
	}
	if strings.Contains(string(out), `"complexity"`) {
		t.Fatalf("%s: bridge must not invent a complexity field; body=%s", name, out)
	}
}

func TestCodexBridgeIgnoresSkillsListReinjectedMidSession(t *testing.T) {
	// Production 2026-11-05: Codex re-sent its skills list at the start of a new coding turn
	// ("开始实现这个需求吧"), a Chat hop delivered it as user text, and its $imagegen mention
	// pinned the turn to image_generation at xhigh. All twelve attempts failed upstream.
	const ask = "开始实现这个需求吧"
	shapes := map[string][]any{
		"separate user message": {testCodexMessage("user", testCodexSkillsInstructions), testCodexMessage("user", ask)},
		"same text part":        {testCodexMessage("user", testCodexSkillsInstructions+"\n\n"+ask)},
		"two parts":             {testCodexMessage("user", testCodexSkillsInstructions, ask)},
		"developer message":     {testCodexMessage("developer", testCodexSkillsInstructions), testCodexMessage("user", ask)},
	}
	for name, tail := range shapes {
		input := append(testCodexPriorHistory(), tail...)
		if got := classifyCodexImageGenerationIntent(testCodexChatBody(t, "xhigh", input...)); got.wantsImage() {
			t.Fatalf("%s: intent = %+v, want none", name, got)
		}

		out := maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, "xhigh", input...), testCodexAuth(), "gpt-6.1-sol", nil)
		assertCodexToolChoiceNotForced(t, name, out)
		if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
			t.Fatalf("%s: reasoning.effort = %q, want xhigh kept for a coding turn; body=%s", name, got, out)
		}
		if requestHasHostedImageGenerationTool(out) {
			t.Fatalf("%s: xhigh coding turn received the hosted image tool; body=%s", name, out)
		}
		if strings.Contains(gjson.GetBytes(out, "instructions").String(), codexImageGenerationBridgeMarker) {
			t.Fatalf("%s: bridge instructions added without the tool; body=%s", name, out)
		}

		// Any explicit effort keeps the tool off a coding turn: the backend would hand it to the
		// image call as complexity, where every value fails.
		for _, effort := range []string{"high", "medium", "low"} {
			out = maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, effort, input...), testCodexAuth(), "gpt-6.1-sol", nil)
			if requestHasHostedImageGenerationTool(out) || gjson.GetBytes(out, "reasoning.effort").String() != effort {
				t.Fatalf("%s/%s: coding turn changed; body=%s", name, effort, out)
			}
		}

		// Without an effort the tool still rides along, unforced.
		out = maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, "", input...), testCodexAuth(), "gpt-6.1-sol", nil)
		if !requestHasHostedImageGenerationTool(out) {
			t.Fatalf("%s: effort-less turn lost the hosted tool; body=%s", name, out)
		}
		assertCodexToolChoiceNotForced(t, name+" (no effort)", out)
	}
}

func TestCodexBridgeFollowUpTurnAfterFailedAttemptsStaysUnforced(t *testing.T) {
	// The user's "继续任务" turn: the previous turn left the skills list beside its ask and six
	// partial assistant replies from the failed attempts.
	input := append(testCodexPriorHistory(), testCodexMessage("user", testCodexSkillsInstructions, "开始实现这个需求吧"))
	for i := 0; i < 6; i++ {
		input = append(input, testCodexMessage("assistant", "继续。我先检查工作区和现有实现。"))
	}
	input = append(input, testCodexMessage("user", "继续任务"))
	out := maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, "xhigh", input...), testCodexAuth(), "gpt-6.1-sol", nil)
	assertCodexToolChoiceNotForced(t, "follow-up turn", out)
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, out)
	}
}

func TestCodexBridgeIgnoresWordsInsideInjectedContext(t *testing.T) {
	// Each block mentions $imagegen or an image keyword; none of it is the user's request. The
	// block shares the user's message (one text part, or a part of its own) the way a Chat hop
	// merges it, so the turn anchor cannot skip it and only the block stripping keeps it out.
	const ask = "把切换按钮放到右上角"
	for name, text := range map[string]string{
		"skills list":         testCodexSkillsInstructions,
		"AGENTS.md":           testCodexAgentsMd,
		"user_instructions":   "<user_instructions>\n需要时用 $imagegen 生图\n</user_instructions>",
		"environment context": testCodexEnvContext,
		"collaboration mode":  "<collaboration_mode>Plan mode. Do not generate image assets.\n</collaboration_mode>",
		"model switch":        "<model_switch>\nThe user switched to an image generation capable model.\n</model_switch>",
		"turn aborted":        "<turn_aborted>\nThe user interrupted: draw a cat\n</turn_aborted>",
		"shell command":       "<user_shell_command>\n$ ls 出图\n</user_shell_command>",
		"image tag":           testCodexImageTag,
		"bridge echo":         codexImageGenerationBridgeText,
		"unclosed block":      "<skills_instructions>\n- hatch-pet: composes the installed $imagegen system skill",
	} {
		for shape, message := range map[string]map[string]any{
			"one part":  testCodexMessage("user", text+"\n\n"+ask),
			"two parts": testCodexMessage("user", text, ask),
		} {
			body := testCodexChatBody(t, "high", testCodexMessage("assistant", "上一轮的回复"), message)
			if got := classifyCodexImageGenerationIntent(body); got.wantsImage() {
				t.Fatalf("%s/%s: intent = %+v, want none", name, shape, got)
			}
		}
	}
}

func TestCodexBridgeExplicitImagegenForcesToolWithoutEffort(t *testing.T) {
	for name, input := range map[string][]any{
		// Verbatim shape of a Codex CLI turn that invoked the skill (2026-09 rollouts).
		"skill link and block": {
			testCodexMessage("user", "[$imagegen](/Users/x/.codex/skills/.system/imagegen/SKILL.md) 帮我画一个小猫咪"),
			testCodexMessage("user", testCodexImagegenSkill),
		},
		"skill block only":     {testCodexMessage("user", testCodexImagegenSkill)},
		"typed mention":        {testCodexMessage("user", "用 $imagegen 做一张登录页插画")},
		"after skills refresh": {testCodexMessage("user", testCodexSkillsInstructions), testCodexMessage("user", "$imagegen a red fox, flat style")},
	} {
		input = append(testCodexPriorHistory(), input...)
		// Production rejected every one of these as complexity on 2026-11-05.
		for _, effort := range []string{"xhigh", "max", "high", "medium", "low"} {
			out := maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, effort, input...), testCodexAuth(), "gpt-6.1-sol", nil)
			if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
				t.Fatalf("%s/%s: tool_choice.type = %q, want image_generation; body=%s", name, effort, got, out)
			}
			assertCodexImageCallWithoutEffort(t, name+"/"+effort, out)
		}
	}

	// A reasoning object that held only the effort goes away with it.
	stringInput := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"xhigh"},"input":"$imagegen a red fox"}`)
	out := maybeEnsureCodexImageGenerationTool(stringInput, testCodexAuth(), "gpt-6.1-sol", nil)
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("string input: tool_choice.type = %q, want image_generation; body=%s", got, out)
	}
	if gjson.GetBytes(out, "reasoning").Exists() {
		t.Fatalf("string input: empty reasoning object left behind; body=%s", out)
	}
}

func TestCodexBridgeKeywordAttachesToolWithoutForcing(t *testing.T) {
	for _, effort := range []string{"xhigh", "medium"} {
		input := append(testCodexPriorHistory(), testCodexMessage("user", "帮我生成一张猫的图片"))
		out := maybeEnsureCodexImageGenerationTool(testCodexChatBody(t, effort, input...), testCodexAuth(), "gpt-6.1-sol", nil)
		assertCodexToolChoiceNotForced(t, effort, out)
		assertCodexImageCallWithoutEffort(t, "keyword/"+effort, out)
	}
}

func TestCodexBridgeSkillComposingImagegenAttachesTool(t *testing.T) {
	// A skill invoked this turn whose instructions call $imagegen is likely to draw: it gets
	// the tool, unforced, because it may run its own scripts first.
	input := append(testCodexPriorHistory(),
		testCodexMessage("user", "$hatch-pet 做一只橘猫桌宠"),
		testCodexMessage("user", "<skill>\n<name>hatch-pet</name>\n<path>/Users/x/.codex/skills/hatch-pet/SKILL.md</path>\nUse $imagegen for every sprite frame, then run scripts/pack.py.\n</skill>"),
	)
	body := testCodexChatBody(t, "xhigh", input...)
	if got := classifyCodexImageGenerationIntent(body); got.level != codexImageIntentKeyword {
		t.Fatalf("intent = %+v, want keyword", got)
	}
	out := maybeEnsureCodexImageGenerationTool(body, testCodexAuth(), "gpt-6.1-sol", nil)
	assertCodexToolChoiceNotForced(t, "composing skill", out)
	assertCodexImageCallWithoutEffort(t, "composing skill", out)
}

func TestCodexBridgeChineseCodingNounsAreNotImageRequests(t *testing.T) {
	for text, want := range map[string]codexImageIntentLevel{
		"修改图标颜色":       codexImageIntentNone,
		"把数据导出图表":      codexImageIntentNone,
		"原生图层顺序不对":     codexImageIntentNone,
		"修图形对齐的 bug":   codexImageIntentNone,
		"返回站外和墙体按钮调整":  codexImageIntentNone,
		"帮我出图":         codexImageIntentKeyword,
		"出图层级不对，再出图一次": codexImageIntentKeyword,
		"改图：背景换成白色":    codexImageIntentKeyword,
		"draw a cat":   codexImageIntentKeyword,
	} {
		if got, _ := gradeCodexUserText(text); got != want {
			t.Fatalf("%q: level = %d, want %d", text, got, want)
		}
	}
}

func TestCodexBridgeEarlierTurnInvocationDoesNotCarryOver(t *testing.T) {
	// Invoking $imagegen once used to pin the next turns too, while it stayed in the scan window.
	invocation := []any{
		testCodexMessage("user", "[$imagegen](/Users/x/.codex/skills/.system/imagegen/SKILL.md) 画一只猫"),
		testCodexMessage("user", testCodexImagegenSkill),
	}
	for name, rest := range map[string][]any{
		"after the image was made": {
			map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed"},
			testCodexMessage("assistant", "已生成。"),
			testCodexMessage("user", "把切换按钮改成蓝色"),
		},
		// An interrupted image turn leaves no model output between the old ask and the new one.
		"after an interrupted turn": {
			testCodexMessage("developer", "<turn_aborted>\nThe user interrupted the previous turn.\n</turn_aborted>"),
			testCodexMessage("user", "算了，先把切换按钮改成蓝色"),
		},
	} {
		input := append(append([]any{}, invocation...), rest...)
		body := testCodexChatBody(t, "xhigh", input...)
		if got := classifyCodexImageGenerationIntent(body); got.wantsImage() {
			t.Fatalf("%s: intent = %+v, want none", name, got)
		}
		out := maybeEnsureCodexImageGenerationTool(body, testCodexAuth(), "gpt-6.1-sol", nil)
		assertCodexToolChoiceNotForced(t, name, out)
		if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
			t.Fatalf("%s: reasoning.effort = %q, want xhigh; body=%s", name, got, out)
		}
	}
}

func TestCodexBridgeTurnKeepsContextSentAfterTheAsk(t *testing.T) {
	// Codex can append context after the user's words (the invoked skill, a refreshed
	// environment): the turn still belongs to the ask before it.
	input := []any{
		testCodexMessage("assistant", "上一轮的回复"),
		testCodexMessage("user", "[$imagegen](/Users/x/.codex/skills/.system/imagegen/SKILL.md) 画一只猫"),
		testCodexMessage("user", testCodexImagegenSkill),
		testCodexMessage("user", testCodexEnvContext),
	}
	if got := classifyCodexImageGenerationIntent(testCodexChatBody(t, "medium", input...)); got.level != codexImageIntentExplicit || got.turnServed {
		t.Fatalf("intent = %+v, want explicit and open", got)
	}

	// An attached image announces itself in parts of its own; the ask is still found.
	withImage := []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": testCodexImageTag},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="},
		map[string]any{"type": "input_text", "text": "</image>"},
	}}, testCodexMessage("user", "这个按钮的位置不对，帮我改一下样式")}
	if got := classifyCodexImageGenerationIntent(testCodexChatBody(t, "xhigh", withImage...)); got.wantsImage() {
		t.Fatalf("image announcement read as a request: %+v", got)
	}
	if _, typed := gradeCodexUserText("</image>"); typed {
		t.Fatal("a stray </image> part counted as user text")
	}
}

func TestCodexBridgeDoesNotRepinAServedTurn(t *testing.T) {
	invocation := []any{
		testCodexMessage("user", "[$imagegen](/Users/x/.codex/skills/.system/imagegen/SKILL.md) 画一只猫，然后保存到 assets/"),
		testCodexMessage("user", testCodexImagegenSkill),
	}
	for name, after := range map[string][]any{
		"after image call": {map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed"}},
		"after tool round": testCodexToolRound("call_9"),
		"after reasoning":  {map[string]any{"type": "reasoning", "summary": []any{}}},
	} {
		input := append(append([]any{}, invocation...), after...)
		body := testCodexChatBody(t, "medium", input...)
		intent := classifyCodexImageGenerationIntent(body)
		if intent.level != codexImageIntentExplicit || !intent.turnServed {
			t.Fatalf("%s: intent = %+v, want explicit and served", name, intent)
		}
		out := maybeEnsureCodexImageGenerationTool(body, testCodexAuth(), "gpt-6.1-sol", nil)
		assertCodexToolChoiceNotForced(t, name, out)
		assertCodexImageCallWithoutEffort(t, name, out)
	}

	// Lite additional_tools after the ask is client context, not model output.
	lite := append(append([]any{}, invocation...), map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{}})
	if intent := classifyCodexImageGenerationIntent(testCodexChatBody(t, "medium", lite...)); intent.turnServed {
		t.Fatalf("additional_tools counted as model output: %+v", intent)
	}
}

func TestCodexBridgeDropsEffortWhenClientToolWillBeCalled(t *testing.T) {
	// The client brought the hosted tool itself and the account opted out of the bridge: an
	// image call that is certain (pinned) or asked for still must not carry the effort.
	optOut := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"codex_image_generation_bridge": false}}
	for name, body := range map[string]string{
		"pinned by client": `{"model":"gpt-6.1-sol","reasoning":{"effort":"high","summary":"detailed"},"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"},"input":"a lighthouse at dusk"}`,
		"asked by user":    `{"model":"gpt-6.1-sol","reasoning":{"effort":"xhigh","summary":"detailed"},"tools":[{"type":"image_generation"}],"input":"帮我生成一张灯塔的图片"}`,
	} {
		assertCodexImageCallWithoutEffort(t, name, maybeEnsureCodexImageGenerationTool([]byte(body), optOut, "gpt-6.1-sol", nil))
	}

	// The client attached the tool to a coding turn: nothing calls it, the effort stays.
	coding := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"xhigh"},"tools":[{"type":"image_generation"}],"input":"refactor the wall toggle"}`)
	if out := maybeEnsureCodexImageGenerationTool(coding, optOut, "gpt-6.1-sol", nil); gjson.GetBytes(out, "reasoning.effort").String() != "xhigh" {
		t.Fatalf("coding turn with a client tool lost its effort; body=%s", out)
	}

	// A request that leaves without the hosted tool keeps whatever effort it had.
	plain := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"xhigh"},"tools":[{"type":"function","name":"shell"}],"input":"refactor"}`)
	if out := maybeEnsureCodexImageGenerationTool(plain, optOut, "gpt-6.1-sol", nil); gjson.GetBytes(out, "reasoning.effort").String() != "xhigh" {
		t.Fatalf("opt-out plain request changed effort; body=%s", out)
	}
	out := maybeEnsureCodexImageGenerationTool(plain, testCodexAuth(), "gpt-6.1-sol", nil)
	if requestHasHostedImageGenerationTool(out) || gjson.GetBytes(out, "reasoning.effort").String() != "xhigh" {
		t.Fatalf("bridge changed a plain xhigh request; body=%s", out)
	}

	// Desktop's local image_gen takes the /v1/images path; the hosted tool is stripped, so the
	// conversation request keeps xhigh.
	local := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"xhigh"},"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]},{"type":"image_generation"}],"input":"$imagegen a cat"}`)
	out = maybeEnsureCodexImageGenerationTool(local, testCodexAuth(), "gpt-6.1-sol", nil)
	if requestHasHostedImageGenerationTool(out) {
		t.Fatalf("hosted tool kept beside local image_gen; body=%s", out)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("local image_gen: reasoning.effort = %q, want xhigh; body=%s", got, out)
	}
}

func TestCodexBridgeLiteKeywordTurnGetsUnforcedTool(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexResponsesLiteHeader, "true")
	body := testCodexChatBody(t, "medium", testCodexMessage("user", "generate an image of a lighthouse"))
	out := maybeEnsureCodexImageGenerationTool(body, testCodexAuth(), "gpt-6.1-sol", headers)
	assertCodexToolChoiceNotForced(t, "lite keyword", out)
	assertCodexImageCallWithoutEffort(t, "lite keyword", out)
}

func TestBuildCodexImageResponsesRequestSendsNoReasoningEffort(t *testing.T) {
	// /v1/images pins tool_choice to the image tool, so any effort it sent became complexity and
	// failed upstream; it used to send medium.
	body, err := buildCodexImageResponsesRequest(&codexImageRequest{Prompt: "a lighthouse", N: 1}, codexImageModel, "gpt-carrier-under-test")
	if err != nil {
		t.Fatalf("buildCodexImageResponsesRequest() error = %v", err)
	}
	if gjson.GetBytes(body, "reasoning.effort").Exists() {
		t.Fatalf("/v1/images request still sends reasoning.effort; body=%s", body)
	}
	if got := gjson.GetBytes(body, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; body=%s", got, body)
	}
}

func TestMentionsImagegenSkill(t *testing.T) {
	for text, want := range map[string]bool{
		"$imagegen":                         true,
		"[$imagegen](/x/imagegen/SKILL.md)": true,
		"use $imagegen to draw":             true,
		"$imagegen-batch 30 frames":         false,
		"$imagegen_v2":                      false,
		"imagegen without the sigil":        false,
		"$imagegen-batch then $imagegen":    true,
	} {
		if got := mentionsImagegenSkill(text); got != want {
			t.Fatalf("mentionsImagegenSkill(%q) = %v, want %v", text, got, want)
		}
	}
}
