package contentmoderation

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// moderatableRoles are the request roles whose text the caller fully controls.
// Assistant/model turns are excluded: they are upstream output being replayed,
// and blocking them would break legitimate multi-turn conversations.
var moderatableRoles = map[string]bool{
	"user":      true,
	"system":    true,
	"developer": true,
	"human":     true,
}

// ExtractModeratableText returns every caller-controlled text fragment in the
// request, joined into a single string for moderation.
//
// It must cover the whole request, not just the newest turn: a client controls
// the entire payload, so moderating only the last message lets an attacker put
// banned text in an earlier turn (or in a system prompt) and append an empty or
// image-only final message to slip past pre_block.
func ExtractModeratableText(format sdktranslator.Format, payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	var parts []string
	switch format {
	case sdktranslator.FormatOpenAIResponse:
		collectResponsesInput(gjson.GetBytes(payload, "input"), &parts)
		collectTextValue(gjson.GetBytes(payload, "instructions"), &parts, false)
	case sdktranslator.FormatClaude:
		// Claude carries the system prompt outside messages; it is caller-controlled too.
		collectTextValue(gjson.GetBytes(payload, "system"), &parts, true)
		collectRoleContent(gjson.GetBytes(payload, "messages"), &parts, true)
	case sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI:
		collectGeminiContent(gjson.GetBytes(payload, "systemInstruction"), &parts)
		collectGeminiContents(gjson.GetBytes(payload, "contents"), &parts)
	default:
		collectRoleContent(gjson.GetBytes(payload, "messages"), &parts, false)
		// Image generation endpoints carry the prompt at the top level.
		if prompt := gjson.GetBytes(payload, "prompt"); prompt.Type == gjson.String {
			addText(&parts, prompt.String(), false)
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, "\n")), " ")
}

func collectRoleContent(messages gjson.Result, parts *[]string, skipSystemReminder bool) {
	if !messages.IsArray() {
		return
	}
	for _, item := range messages.Array() {
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if !moderatableRoles[role] {
			continue
		}
		collectTextValue(item.Get("content"), parts, skipSystemReminder)
	}
}

func collectResponsesInput(input gjson.Result, parts *[]string) {
	switch {
	case !input.Exists():
		return
	case input.Type == gjson.String:
		addText(parts, input.String(), false)
	case input.IsArray():
		for _, item := range input.Array() {
			collectResponsesInputItem(item, parts)
		}
	case input.IsObject():
		collectResponsesInputItem(input, parts)
	}
}

func collectResponsesInputItem(item gjson.Result, parts *[]string) {
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	typeName := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	// Items without a role but typed input_text are caller input as well.
	if !moderatableRoles[role] && typeName != "input_text" {
		return
	}
	collectTextValue(item.Get("content"), parts, false)
	if typeName == "input_text" || item.Get("text").Exists() {
		collectTextValue(item, parts, false)
	}
}

func collectGeminiContents(contents gjson.Result, parts *[]string) {
	if !contents.IsArray() {
		return
	}
	for _, item := range contents.Array() {
		collectGeminiContent(item, parts)
	}
}

func collectGeminiContent(content gjson.Result, parts *[]string) {
	if !content.Exists() {
		return
	}
	role := strings.ToLower(strings.TrimSpace(content.Get("role").String()))
	// Gemini omits the role on single-turn and systemInstruction payloads.
	if role != "" && !moderatableRoles[role] {
		return
	}
	if values := content.Get("parts"); values.IsArray() {
		for _, part := range values.Array() {
			addText(parts, part.Get("text").String(), false)
		}
	}
}

func collectTextValue(value gjson.Result, parts *[]string, skipSystemReminder bool) {
	switch {
	case !value.Exists():
		return
	case value.Type == gjson.String:
		addText(parts, value.String(), skipSystemReminder)
	case value.IsArray():
		for _, item := range value.Array() {
			collectTextValue(item, parts, skipSystemReminder)
		}
	case value.IsObject():
		typeName := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		if typeName == "" || typeName == "text" || typeName == "input_text" || typeName == "message" {
			addText(parts, value.Get("text").String(), skipSystemReminder)
			collectTextValue(value.Get("content"), parts, skipSystemReminder)
		}
	}
}

func addText(parts *[]string, value string, skipSystemReminder bool) {
	value = strings.TrimSpace(value)
	// <system-reminder> blocks are client-injected boilerplate, not caller prose.
	if value == "" || (skipSystemReminder && strings.HasPrefix(value, "<system-reminder>")) {
		return
	}
	*parts = append(*parts, value)
}
