package common

import (
	"strconv"
	"strings"
)

// The native structured output fields are only written for models documented
// to take them. An upstream that does not know a field fails the whole request
// with a 400, so a request that used to be served (with its JSON requirement
// dropped) would start failing. Every other model is asked in the prompt.
//
// Where the lists come from, as of 2026-10; check them again when a model
// family is added:
//   - Anthropic: the supportedModels of
//     https://platform.claude.com/docs/en/build-with-claude/structured-outputs
//     are Claude Sonnet 4.5, Opus 4.5 and Haiku 4.5, every later Opus, Sonnet
//     and Haiku, and every Fable and Mythos model. Claude Sonnet 4, Opus 4,
//     Opus 4.1 and the Claude 3 models are not on it.
//   - Gemini: responseJsonSchema from Gemini 2.5 on, and JSON mode
//     (responseMimeType application/json) on every Gemini text model, per
//     https://ai.google.dev/gemini-api/docs/structured-output and the
//     capability tables under https://ai.google.dev/gemini-api/docs/models
//     (gemini-2.5-flash-image, for one, lists structured outputs as not
//     supported).

// normalizeModelID reduces a model name to the bare id as its vendor writes
// it: lower case, without a path (models/, publishers/anthropic/models/,
// anthropic/ on aggregators, a Bedrock ARN), without the Bedrock region and
// vendor prefix (us.anthropic., anthropic.), and without what follows an @, a
// colon or a parenthesis (a Vertex version, a Bedrock -v1:0 revision, a tag,
// a thinking suffix).
func normalizeModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	if i := strings.IndexAny(id, "@:("); i >= 0 {
		id = id[:i]
	}
	return id
}

// claudeModelTakesOutputFormat reports whether an Anthropic model takes
// output_config.format: Opus, Sonnet and Haiku from 4.5 on, and every Fable
// and Mythos model. Names that are not Claude's (kimi, glm, minimax, deepseek
// behind a Claude-format upstream) and names it cannot read do not.
func claudeModelTakesOutputFormat(model string) bool {
	family, major, minor, versioned := claudeModelVersion(model)
	switch family {
	case "fable", "mythos":
		return true
	case "opus", "sonnet", "haiku":
		return versioned && (major > 4 || major == 4 && minor >= 5)
	}
	return false
}

// claudeModelVersion reads the family and version of a Claude model id in
// either naming scheme: claude-<family>-<major>[-<minor>][-<date>] from the
// Claude 4 models on (claude-sonnet-4-5-20250929, claude-opus-4-20250514), and
// claude-<major>[-<minor>]-<family> before them (claude-3-5-sonnet-20241022).
// A minor version is one or two digits, so the date in
// claude-sonnet-4-20250514 is not read as one: that model is 4.0. Dotted
// versions (claude-sonnet-4.5) are read too.
func claudeModelVersion(model string) (family string, major, minor int, versioned bool) {
	rest, ok := strings.CutPrefix(normalizeModelID(model), "claude-")
	if !ok {
		return "", 0, 0, false
	}
	tokens := strings.Split(rest, "-")
	if major, minor, used, ok := modelVersion(tokens); ok {
		// claude-3-5-sonnet: the family follows the version.
		if used < len(tokens) {
			family = tokens[used]
		}
		return family, major, minor, true
	}
	// claude-sonnet-4-5: the version follows the family.
	family = tokens[0]
	major, minor, _, versioned = modelVersion(tokens[1:])
	return family, major, minor, versioned
}

// geminiOutputSupport is how much of a structured output requirement a model
// behind a Gemini-format upstream takes in its own fields.
type geminiOutputSupport int

const (
	// geminiAskInSystem is not a Gemini model (Antigravity also serves Claude
	// and gpt-oss): the requirement goes into systemInstruction.
	geminiAskInSystem geminiOutputSupport = iota
	// geminiJSONMode is a Gemini model before 2.5: responseMimeType, with a
	// schema asked for in systemInstruction.
	geminiJSONMode
	// geminiJSONSchema is Gemini 2.5 or later: responseMimeType and
	// responseJsonSchema.
	geminiJSONSchema
	// geminiAskInUserTurn is Gemma, which the Gemini API serves without
	// systemInstruction ("Developer instruction is not enabled"): the
	// requirement goes at the start of the first user turn, where Google's
	// Gemma guide puts system-level instructions.
	geminiAskInUserTurn
	// geminiNoTextOutput is a model that answers with images, speech or
	// embeddings rather than text. There is no JSON to ask for, and their
	// capabilities list structured output as unsupported, so the request is
	// left as it came.
	geminiNoTextOutput
)

// geminiStructuredOutputSupport classifies model for WriteGemini.
func geminiStructuredOutputSupport(model string) geminiOutputSupport {
	id := normalizeModelID(model)
	if strings.HasPrefix(id, "gemma-") {
		return geminiAskInUserTurn
	}
	rest, ok := strings.CutPrefix(id, "gemini-")
	if !ok {
		return geminiAskInSystem
	}
	tokens := strings.Split(rest, "-")
	for _, token := range tokens {
		switch token {
		case "image", "tts", "audio", "live", "embedding":
			return geminiNoTextOutput
		}
	}
	if major, minor, _, ok := modelVersion(tokens); ok {
		if major > 2 || major == 2 && minor >= 5 {
			return geminiJSONSchema
		}
		return geminiJSONMode
	}
	// The unversioned aliases (gemini-flash-latest, gemini-pro-latest) name
	// the current models.
	if tokens[len(tokens)-1] == "latest" {
		return geminiJSONSchema
	}
	return geminiJSONMode
}

// modelVersion reads a version from the start of tokens, a model id split at
// its dashes: "4", "5" (major and minor), or "4.5". used is the number of
// tokens it took.
func modelVersion(tokens []string) (major, minor, used int, ok bool) {
	if len(tokens) == 0 {
		return 0, 0, 0, false
	}
	if majorText, minorText, dotted := strings.Cut(tokens[0], "."); dotted {
		major, okMajor := versionNumber(majorText)
		minor, okMinor := versionNumber(minorText)
		if !okMajor || !okMinor {
			return 0, 0, 0, false
		}
		return major, minor, 1, true
	}
	major, ok = versionNumber(tokens[0])
	if !ok {
		return 0, 0, 0, false
	}
	if len(tokens) > 1 {
		if minor, ok := versionNumber(tokens[1]); ok {
			return major, minor, 2, true
		}
	}
	return major, 0, 1, true
}

// versionNumber reads a version number of one or two digits. Longer runs of
// digits in a model id are dates (20250514) or revisions, not versions.
func versionNumber(text string) (int, bool) {
	if len(text) == 0 || len(text) > 2 {
		return 0, false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil
}
