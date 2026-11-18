// Package gemini provides request translation functionality for Gemini CLI to Gemini API compatibility.
// It handles parsing and transforming Gemini CLI API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini CLI API format and Gemini API's expected format.
package gemini

import (
	"fmt"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToGeminiCLI parses and transforms a Gemini CLI API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Gemini CLI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToGeminiCLI(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	template := ""
	template = `{"project":"","request":{},"model":""}`
	template, _ = sjson.SetRaw(template, "request", string(rawJSON))
	template, _ = sjson.Set(template, "model", gjson.Get(template, "request.model").String())
	template, _ = sjson.Delete(template, "request.model")

	template, errFixCLIToolResponse := fixCLIToolResponse(template)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}

	systemInstructionResult := gjson.Get(template, "request.system_instruction")
	if systemInstructionResult.Exists() {
		template, _ = sjson.SetRaw(template, "request.systemInstruction", systemInstructionResult.Raw)
		template, _ = sjson.Delete(template, "request.system_instruction")
	}
	rawJSON = []byte(template)

	// The three edits below used to rewrite the whole request once per
	// content, declaration and part, which grew with the square of a long
	// session. Each now patches the elements it changes in one pass (see
	// common.RawPatch), in the order they always ran.
	rawJSON = common.NormalizeContentRoles(rawJSON, "request.contents")
	rawJSON = common.RenameFunctionDeclarationParameters(rawJSON, "request.tools")
	rawJSON = common.SkipThoughtSignatureValidation(rawJSON, "request.contents")

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

// FunctionCallGroup represents a group of function calls and their responses
type FunctionCallGroup struct {
	ResponsesNeeded int
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON string to be processed
//
// Returns:
//   - string: The processed JSON string with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input string) (string, error) {
	// Parse the input JSON to extract the conversation structure
	parsed := gjson.Parse(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		// log.Debugf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	// Initialize data structures for processing and grouping. The new contents
	// array is built once instead of appending to a wrapper document per
	// content, which re-parsed and copied everything appended so far each time.
	var newContents translatorcommon.JSONArrayBuilder
	var pendingGroups []*FunctionCallGroup // Groups awaiting completion with responses
	var collectedResponses []gjson.Result  // Standalone responses to be matched

	// Process each content object in the conversation
	// This iterates through messages and groups function calls with their responses
	contents.ForEach(func(key, value gjson.Result) bool {
		role := value.Get("role").String()
		parts := value.Get("parts")

		// Check if this content has function responses
		var responsePartsInThisContent []gjson.Result
		parts.ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionResponse").Exists() {
				responsePartsInThisContent = append(responsePartsInThisContent, part)
			}
			return true
		})

		// If this content has function responses, collect them
		if len(responsePartsInThisContent) > 0 {
			collectedResponses = append(collectedResponses, responsePartsInThisContent...)

			// Check if any pending groups can be satisfied
			for i := len(pendingGroups) - 1; i >= 0; i-- {
				group := pendingGroups[i]
				if len(collectedResponses) >= group.ResponsesNeeded {
					// Take the needed responses for this group
					groupResponses := collectedResponses[:group.ResponsesNeeded]
					collectedResponses = collectedResponses[group.ResponsesNeeded:]

					// Create merged function response content
					if merged, ok := mergedFunctionResponseContent(groupResponses); ok {
						newContents.Append(merged)
					}

					// Remove this group as it's been satisfied
					pendingGroups = append(pendingGroups[:i], pendingGroups[i+1:]...)
					break
				}
			}

			return true // Skip adding this content, responses are merged
		}

		// If this is a model with function calls, create a new group
		if role == "model" {
			functionCallsCount := 0
			parts.ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					functionCallsCount++
				}
				return true
			})

			if functionCallsCount > 0 {
				// Add the model content
				if !value.IsObject() {
					log.Warnf("failed to parse model content")
					return true
				}
				newContents.Append(value.Raw)

				// Create a new group for tracking responses
				group := &FunctionCallGroup{
					ResponsesNeeded: functionCallsCount,
				}
				pendingGroups = append(pendingGroups, group)
			} else {
				// Regular model content without function calls
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					return true
				}
				newContents.Append(value.Raw)
			}
		} else {
			// Non-model content (user, etc.)
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				return true
			}
			newContents.Append(value.Raw)
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			if merged, ok := mergedFunctionResponseContent(groupResponses); ok {
				newContents.Append(merged)
			}
		}
	}

	// Update the original JSON with the new contents
	result := input
	result, _ = sjson.SetRaw(result, "request.contents", newContents.String())

	return result, nil
}

// mergedFunctionResponseContent merges a group's responses into one
// function-role content, the same bytes the old per-part sjson appends onto
// {"parts":[],"role":"function"} produced. A response that is not an object
// is skipped with a warning, as before; it reports false when none is left,
// where the old code skipped the content.
func mergedFunctionResponseContent(responses []gjson.Result) (string, bool) {
	var parts translatorcommon.JSONArrayBuilder
	for _, response := range responses {
		if !response.IsObject() {
			log.Warnf("failed to parse function response")
			continue
		}
		parts.Append(response.Raw)
	}
	if parts.Len() == 0 {
		return "", false
	}
	return `{"parts":` + parts.String() + `,"role":"function"}`, true
}
