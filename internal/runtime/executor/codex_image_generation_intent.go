package executor

import (
	"strings"

	"github.com/tidwall/gjson"
)

// codexImageIntentLevel grades how sure the bridge is that the current turn asks for a picture.
type codexImageIntentLevel int

const (
	// codexImageIntentNone: nothing the user wrote this turn asks for an image.
	codexImageIntentNone codexImageIntentLevel = iota
	// codexImageIntentKeyword: the user's own words read like an image request ("画一只猫",
	// "generate an image"). A keyword is a guess: coding turns say 修改图片 or "draw a line"
	// all the time. It may attach the hosted tool but never pins tool_choice to it.
	codexImageIntentKeyword
	// codexImageIntentExplicit: the user invoked the imagegen skill this turn, by typing
	// $imagegen or because Codex injected that skill's <skill> block for this turn.
	codexImageIntentExplicit
)

// codexImageIntent is what the bridge knows about the latest user turn of a request.
type codexImageIntent struct {
	level codexImageIntentLevel
	// turnServed reports that the model already produced output after the latest user
	// message, so this request continues a turn instead of opening it.
	turnServed bool
}

func (i codexImageIntent) wantsImage() bool { return i.level != codexImageIntentNone }

// forcesToolChoice reports whether the request may pin tool_choice to image_generation.
//
// Pinning takes the turn away from the model: it can call nothing but the image tool. That
// is only right when the user named the imagegen skill, and only on the request that opens
// the turn. Pinning a later request of the same turn would make the model draw again instead
// of finishing whatever it started.
func (i codexImageIntent) forcesToolChoice() bool {
	return i.level == codexImageIntentExplicit && !i.turnServed
}

// codexInjectedContextBlock is a context block a Codex client writes into the conversation
// on the user's behalf. Its text is not something the user typed.
type codexInjectedContextBlock struct {
	open  string // lowercase
	close string // lowercase
}

const codexSkillBlockOpen = "<skill>"

// codexInjectedContextBlocks lists the blocks Codex (0.16x CLI and Desktop) injects into
// input. Most arrive as developer messages, but a client that relays Codex through a Chat
// Completions hop can deliver them inside user messages: on 2026-11-05 a Codex session behind
// a Responses-to-Chat proxy had its mid-session <skills_instructions> refresh arrive that way,
// and one third-party skill's description in that list ("composes the installed $imagegen
// system skill") was read as an image request. Every coding turn of the session was then
// pinned to image_generation and failed upstream. So the user's words are what is left after
// these blocks are cut out, whatever role carried them.
var codexInjectedContextBlocks = []codexInjectedContextBlock{
	{open: "<skills_instructions>", close: "</skills_instructions>"},
	{open: codexSkillBlockOpen, close: "</skill>"},
	{open: "# agents.md instructions for", close: "</instructions>"},
	{open: "<instructions>", close: "</instructions>"},
	{open: "<user_instructions>", close: "</user_instructions>"},
	{open: "<environment_context>", close: "</environment_context>"},
	{open: "<permissions instructions>", close: "</permissions instructions>"},
	{open: "<collaboration_mode>", close: "</collaboration_mode>"},
	{open: "<multi_agent_mode>", close: "</multi_agent_mode>"},
	{open: "<multi_agent_role>", close: "</multi_agent_role>"},
	{open: "<model_switch>", close: "</model_switch>"},
	{open: "<turn_aborted>", close: "</turn_aborted>"},
	{open: "<user_shell_command>", close: "</user_shell_command>"},
	// Attached images are announced as `<image name=[Image #1] path=...>` in a text part of
	// their own, with `</image>` in another part; the file name is not the user's request.
	// The empty close cuts a stray `</image>` part on its own.
	{open: "<image name=", close: "</image>"},
	{open: "</image>", close: ""},
	{open: "<cliproxy-codex-image-generation>", close: "</cliproxy-codex-image-generation>"},
}

// classifyCodexImageGenerationIntent grades the latest user turn of a /responses body.
//
// Only that turn counts. An image asked for three turns ago is history, and treating it as
// current kept every later coding request of the conversation pinned to the image tool. The
// turn is anchored on the latest message that holds words the user typed, and also takes the
// context messages after it: Codex sends the <skill> block of an invoked skill right after
// the user's message. Walking back stops at that anchor, so an earlier ask that sits right
// before it (a turn the user interrupted, say) does not leak into the current one.
func classifyCodexImageGenerationIntent(body []byte) codexImageIntent {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		level, _ := gradeCodexUserText(input.String())
		return codexImageIntent{level: level}
	}
	if !input.IsArray() {
		return codexImageIntent{}
	}
	items := input.Array()
	last := -1
	for i := len(items) - 1; i >= 0; i-- {
		if isCodexMessageItem(items[i]) && codexMessageRole(items[i]) == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return codexImageIntent{}
	}
	intent := codexImageIntent{turnServed: codexItemsContainModelOutput(items[last+1:])}
	for i := last; i >= 0 && intent.level != codexImageIntentExplicit; i-- {
		item := items[i]
		if !isCodexMessageItem(item) {
			break
		}
		role := codexMessageRole(item)
		if role == "assistant" {
			break
		}
		if role != "user" {
			// developer/system text is written by the client, never by the user.
			continue
		}
		typedByUser := false
		for _, text := range codexMessageTexts(item) {
			level, hasUserText := gradeCodexUserText(text)
			if level > intent.level {
				intent.level = level
			}
			typedByUser = typedByUser || hasUserText
		}
		if typedByUser {
			break
		}
	}
	return intent
}

func isCodexMessageItem(item gjson.Result) bool {
	switch strings.TrimSpace(item.Get("type").String()) {
	case "message":
		return true
	case "":
		// EasyInputMessage may omit type and carry only role + content.
		return codexMessageRole(item) != ""
	}
	return false
}

func codexMessageRole(item gjson.Result) string {
	return strings.ToLower(strings.TrimSpace(item.Get("role").String()))
}

// codexItemsContainModelOutput reports whether anything after the latest user message came
// from the model: an assistant message, a tool call or its output, reasoning, or an image
// call. Client context (developer messages, Lite additional_tools) does not count.
func codexItemsContainModelOutput(items []gjson.Result) bool {
	for _, item := range items {
		if isCodexMessageItem(item) {
			if codexMessageRole(item) == "assistant" {
				return true
			}
			continue
		}
		if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			continue
		}
		return true
	}
	return false
}

// codexMessageTexts returns the text parts of one message item, one entry per part, so a
// context block never runs into the user's words across a part boundary.
func codexMessageTexts(item gjson.Result) []string {
	content := item.Get("content")
	if content.Type == gjson.String {
		return []string{content.String()}
	}
	if !content.IsArray() {
		if text := item.Get("text"); text.Type == gjson.String {
			return []string{text.String()}
		}
		return nil
	}
	var texts []string
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "input_text", "output_text", "text":
			if text := part.Get("text"); text.Type == gjson.String && text.String() != "" {
				texts = append(texts, text.String())
			}
		}
	}
	return texts
}

// gradeCodexUserText grades one text part of a user message and reports whether anything
// the user typed is left once the injected context is cut out.
func gradeCodexUserText(text string) (codexImageIntentLevel, bool) {
	if strings.TrimSpace(text) == "" {
		return codexImageIntentNone, false
	}
	userText, level := stripCodexInjectedContext(strings.ToLower(text))
	hasUserText := strings.TrimSpace(userText) != ""
	switch {
	case mentionsImagegenSkill(userText):
		level = codexImageIntentExplicit
	case level == codexImageIntentNone && (looksLikeChineseImageRequest(userText) || looksLikeEnglishImageRequest(userText)):
		level = codexImageIntentKeyword
	}
	return level, hasUserText
}

// stripCodexInjectedContext removes injected context blocks from lowercased text and grades
// the <skill> blocks among them. Codex adds a skill's <skill> block right after the user's
// message that invokes it: the imagegen skill's own block is an explicit request, and another
// skill whose instructions call $imagegen (a sprite or mascot generator) is likely to draw, so
// it counts like a keyword. A skill merely listed in <skills_instructions> counts for nothing.
//
// Blocks are cut in the order they appear, so a block that quotes another block's tag in its
// own text is removed whole. A block whose closing tag is missing runs to the end of the text:
// truncated context must not be read as the user's words.
func stripCodexInjectedContext(lower string) (string, codexImageIntentLevel) {
	var kept strings.Builder
	level := codexImageIntentNone
	rest := lower
	for {
		at, block := firstCodexInjectedContextBlock(rest)
		if at < 0 {
			if kept.Len() == 0 {
				return rest, level
			}
			kept.WriteString(rest)
			return kept.String(), level
		}
		kept.WriteString(rest[:at])
		kept.WriteByte('\n')
		inner := rest[at+len(block.open):]
		rest = ""
		if end := strings.Index(inner, block.close); end >= 0 {
			rest = inner[end+len(block.close):]
			inner = inner[:end]
		}
		if block.open != codexSkillBlockOpen {
			continue
		}
		switch {
		case codexSkillBlockName(inner) == "imagegen":
			level = codexImageIntentExplicit
		case level == codexImageIntentNone && mentionsImagegenSkill(inner):
			level = codexImageIntentKeyword
		}
	}
}

func firstCodexInjectedContextBlock(text string) (int, codexInjectedContextBlock) {
	first := -1
	var found codexInjectedContextBlock
	for _, block := range codexInjectedContextBlocks {
		if at := strings.Index(text, block.open); at >= 0 && (first < 0 || at < first) {
			first, found = at, block
		}
	}
	return first, found
}

// codexSkillBlockName reads the skill name from the inside of a <skill> block, which Codex
// writes as `<name>imagegen</name>` on the block's first line.
func codexSkillBlockName(inner string) string {
	start := strings.Index(inner, "<name>")
	if start < 0 {
		return ""
	}
	rest := inner[start+len("<name>"):]
	end := strings.Index(rest, "</name>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// mentionsImagegenSkill reports a $imagegen skill mention in the user's own words, including
// Desktop's `[$imagegen](.../SKILL.md)` link. A longer skill name that merely starts with
// imagegen ($imagegen-batch) is a different skill.
func mentionsImagegenSkill(lower string) bool {
	const mention = "$imagegen"
	for from := 0; ; {
		at := strings.Index(lower[from:], mention)
		if at < 0 {
			return false
		}
		end := from + at + len(mention)
		if end >= len(lower) || !isSkillNameByte(lower[end]) {
			return true
		}
		from = end
	}
}

func isSkillNameByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

var codexChineseImageKeywords = []string{"画一", "画个", "画只", "画一张", "生成一张", "生成图片", "生图", "出图", "改图", "修图", "绘一张"}

// codexChineseImageNounSuffixes are characters that turn a short keyword ending in 图 into an
// everyday coding noun: 修改图标, 导出图表, 原生图层, 输出图形, 施工图纸, 图例, 图谱, 图元.
var codexChineseImageNounSuffixes = []string{"标", "表", "层", "形", "纸", "例", "谱", "元"}

func looksLikeChineseImageRequest(lower string) bool {
	for _, key := range codexChineseImageKeywords {
		if containsChineseImageKeyword(lower, key) {
			return true
		}
	}
	return false
}

func containsChineseImageKeyword(lower, key string) bool {
	if !strings.HasSuffix(key, "图") {
		return strings.Contains(lower, key)
	}
	for from := 0; from < len(lower); {
		at := strings.Index(lower[from:], key)
		if at < 0 {
			return false
		}
		end := from + at + len(key)
		if !hasAnyPrefix(lower[end:], codexChineseImageNounSuffixes) {
			return true
		}
		from = end
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func looksLikeEnglishImageRequest(lower string) bool {
	keys := []string{
		"generate an image", "generate a image", "draw a ", "draw an ", "create an image",
		"make an image", "edit this image", "image generation", "generate image",
	}
	for _, key := range keys {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}
