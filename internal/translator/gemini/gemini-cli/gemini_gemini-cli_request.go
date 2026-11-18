// Package gemini provides request translation functionality for Claude API.
// It handles parsing and transforming Claude API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude API format and the internal client's expected format.
package geminiCLI

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PrepareClaudeRequest parses and transforms a Claude API request into internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
func ConvertGeminiCLIRequestToGemini(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	modelResult := gjson.GetBytes(rawJSON, "model")
	rawJSON = []byte(gjson.GetBytes(rawJSON, "request").Raw)
	rawJSON, _ = sjson.SetBytes(rawJSON, "model", modelResult.String())
	if gjson.GetBytes(rawJSON, "systemInstruction").Exists() {
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "system_instruction", []byte(gjson.GetBytes(rawJSON, "systemInstruction").Raw))
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "systemInstruction")
	}

	// These two edits used to rewrite the whole request once per declaration
	// and per part, which grew with the square of a long session. Each now
	// patches the elements it changes in one pass (see common.RawPatch).
	rawJSON = common.RenameFunctionDeclarationParameters(rawJSON, "tools")
	rawJSON = common.SkipThoughtSignatureValidation(rawJSON, "contents")

	return common.AttachDefaultSafetySettings(rawJSON, "safetySettings")
}
