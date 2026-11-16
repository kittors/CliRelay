package gemini

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Code below is the translator as it was before the 2026-11-16 rewrite
// (origin/dev 25c4642), kept verbatim except for the function names. The
// equivalence tests compare the current translator against it byte for byte;
// do not change it to match new behaviour.

func convertGeminiRequestToGeminiCLIReference(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	template := ""
	template = `{"project":"","request":{},"model":""}`
	template, _ = sjson.SetRaw(template, "request", string(rawJSON))
	template, _ = sjson.Set(template, "model", gjson.Get(template, "request.model").String())
	template, _ = sjson.Delete(template, "request.model")

	template, errFixCLIToolResponse := fixCLIToolResponseReference(template)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}

	systemInstructionResult := gjson.Get(template, "request.system_instruction")
	if systemInstructionResult.Exists() {
		template, _ = sjson.SetRaw(template, "request.systemInstruction", systemInstructionResult.Raw)
		template, _ = sjson.Delete(template, "request.system_instruction")
	}
	rawJSON = []byte(template)

	// Normalize roles in request.contents: default to valid values if missing/invalid
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if contents.Exists() {
		prevRole := ""
		idx := 0
		contents.ForEach(func(_ gjson.Result, value gjson.Result) bool {
			role := value.Get("role").String()
			valid := role == "user" || role == "model"
			if role == "" || !valid {
				var newRole string
				if prevRole == "" {
					newRole = "user"
				} else if prevRole == "user" {
					newRole = "model"
				} else {
					newRole = "user"
				}
				path := fmt.Sprintf("request.contents.%d.role", idx)
				rawJSON, _ = sjson.SetBytes(rawJSON, path, newRole)
				role = newRole
			}
			prevRole = role
			idx++
			return true
		})
	}

	toolsResult := gjson.GetBytes(rawJSON, "request.tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			functionDeclarationsResult := gjson.GetBytes(rawJSON, fmt.Sprintf("request.tools.%d.function_declarations", i))
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				functionDeclarationsResults := functionDeclarationsResult.Array()
				for j := 0; j < len(functionDeclarationsResults); j++ {
					parametersResult := gjson.GetBytes(rawJSON, fmt.Sprintf("request.tools.%d.function_declarations.%d.parameters", i, j))
					if parametersResult.Exists() {
						strJson, _ := util.RenameKey(string(rawJSON), fmt.Sprintf("request.tools.%d.function_declarations.%d.parameters", i, j), fmt.Sprintf("request.tools.%d.function_declarations.%d.parametersJsonSchema", i, j))
						rawJSON = []byte(strJson)
					}
				}
			}
		}
	}

	gjson.GetBytes(rawJSON, "request.contents").ForEach(func(key, content gjson.Result) bool {
		if content.Get("role").String() == "model" {
			content.Get("parts").ForEach(func(partKey, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", key.Int(), partKey.Int()), "skip_thought_signature_validator")
				} else if part.Get("thoughtSignature").Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", key.Int(), partKey.Int()), "skip_thought_signature_validator")
				}
				return true
			})
		}
		return true
	})

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

func fixCLIToolResponseReference(input string) (string, error) {
	// Parse the input JSON to extract the conversation structure
	parsed := gjson.Parse(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		// log.Debugf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	// Initialize data structures for processing and grouping
	contentsWrapper := `{"contents":[]}`
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
					functionResponseContent := `{"parts":[],"role":"function"}`
					for _, response := range groupResponses {
						if !response.IsObject() {
							log.Warnf("failed to parse function response")
							continue
						}
						functionResponseContent, _ = sjson.SetRaw(functionResponseContent, "parts.-1", response.Raw)
					}

					if gjson.Get(functionResponseContent, "parts.#").Int() > 0 {
						contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", functionResponseContent)
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
				contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)

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
				contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)
			}
		} else {
			// Non-model content (user, etc.)
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				return true
			}
			contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			functionResponseContent := `{"parts":[],"role":"function"}`
			for _, response := range groupResponses {
				if !response.IsObject() {
					log.Warnf("failed to parse function response")
					continue
				}
				functionResponseContent, _ = sjson.SetRaw(functionResponseContent, "parts.-1", response.Raw)
			}

			if gjson.Get(functionResponseContent, "parts.#").Int() > 0 {
				contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", functionResponseContent)
			}
		}
	}

	// Update the original JSON with the new contents
	result := input
	result, _ = sjson.SetRaw(result, "request.contents", gjson.Get(contentsWrapper, "contents").Raw)

	return result, nil
}
