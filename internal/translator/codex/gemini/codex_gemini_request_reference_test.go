package gemini

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Code below is the translator as it was before the 2026-11-16 rewrite
// (origin/dev 25c4642), kept verbatim except for the function names. The
// equivalence tests compare the current translator against it byte for byte;
// do not change it to match new behaviour.

func convertGeminiRequestToCodexReference(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Base template
	out := `{"model":"","instructions":"","input":[]}`

	root := gjson.ParseBytes(rawJSON)

	// Pre-compute tool name shortening map from declared functionDeclarations
	shortMap := map[string]string{}
	if tools := root.Get("tools"); tools.IsArray() {
		var names []string
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			fns := tarr[i].Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			for _, fn := range fns.Array() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
		if len(names) > 0 {
			shortMap = buildShortNameMap(names)
		}
	}

	// helper for generating paired call IDs in the form: call_<alphanum>
	// Gemini uses sequential pairing across possibly multiple in-flight
	// functionCalls, so we keep a FIFO queue of generated call IDs and
	// consume them in order when functionResponses arrive.
	var pendingCallIDs []string

	// genCallID creates a random call id like: call_<8chars>
	genCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		// 8 chars random suffix
		for i := 0; i < 24; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "call_" + b.String()
	}

	// Model
	out, _ = sjson.Set(out, "model", modelName)

	// System instruction -> as a user message with input_text parts
	sysParts := root.Get("system_instruction.parts")
	if sysParts.IsArray() {
		msg := `{"type":"message","role":"developer","content":[]}`
		arr := sysParts.Array()
		for i := 0; i < len(arr); i++ {
			p := arr[i]
			if t := p.Get("text"); t.Exists() {
				part := `{}`
				part, _ = sjson.Set(part, "type", "input_text")
				part, _ = sjson.Set(part, "text", t.String())
				msg, _ = sjson.SetRaw(msg, "content.-1", part)
			}
		}
		if len(gjson.Get(msg, "content").Array()) > 0 {
			out, _ = sjson.SetRaw(out, "input.-1", msg)
		}
	}

	// Contents -> messages and function calls/results
	contents := root.Get("contents")
	if contents.IsArray() {
		items := contents.Array()
		for i := 0; i < len(items); i++ {
			item := items[i]
			role := item.Get("role").String()
			if role == "model" {
				role = "assistant"
			}

			parts := item.Get("parts")
			if !parts.IsArray() {
				continue
			}
			parr := parts.Array()
			for j := 0; j < len(parr); j++ {
				p := parr[j]
				// text part
				if t := p.Get("text"); t.Exists() {
					msg := `{"type":"message","role":"","content":[]}`
					msg, _ = sjson.Set(msg, "role", role)
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := `{}`
					part, _ = sjson.Set(part, "type", partType)
					part, _ = sjson.Set(part, "text", t.String())
					msg, _ = sjson.SetRaw(msg, "content.-1", part)
					out, _ = sjson.SetRaw(out, "input.-1", msg)
					continue
				}

				// function call from model
				if fc := p.Get("functionCall"); fc.Exists() {
					fn := `{"type":"function_call"}`
					if name := fc.Get("name"); name.Exists() {
						n := name.String()
						if short, ok := shortMap[n]; ok {
							n = short
						} else {
							n = shortenNameIfNeeded(n)
						}
						fn, _ = sjson.Set(fn, "name", n)
					}
					if args := fc.Get("args"); args.Exists() {
						fn, _ = sjson.Set(fn, "arguments", args.Raw)
					}
					// generate a paired random call_id and enqueue it so the
					// corresponding functionResponse can pop the earliest id
					// to preserve ordering when multiple calls are present.
					id := genCallID()
					fn, _ = sjson.Set(fn, "call_id", id)
					pendingCallIDs = append(pendingCallIDs, id)
					out, _ = sjson.SetRaw(out, "input.-1", fn)
					continue
				}

				// function response from user
				if fr := p.Get("functionResponse"); fr.Exists() {
					fno := `{"type":"function_call_output"}`
					parsed := translatorcommon.ParseGeminiFunctionResponse(fr)
					if parsed.HasImage() {
						fno, _ = sjson.SetRaw(fno, "output", translatorcommon.ToResponsesOutput(parsed))
					} else if len(parsed.Parts) > 0 {
						fno, _ = sjson.Set(fno, "output", parsed.Text())
					} else if parsed.Raw.Exists() {
						fno, _ = sjson.Set(fno, "output", parsed.Text())
					}
					// fno, _ = sjson.Set(fno, "call_id", "call_W6nRJzFXyPM2LFBbfo98qAbq")
					// attach the oldest queued call_id to pair the response
					// with its call. If the queue is empty, generate a new id.
					var id string
					if len(pendingCallIDs) > 0 {
						id = pendingCallIDs[0]
						// pop the first element
						pendingCallIDs = pendingCallIDs[1:]
					} else {
						id = genCallID()
					}
					fno, _ = sjson.Set(fno, "call_id", id)
					out, _ = sjson.SetRaw(out, "input.-1", fno)
					continue
				}
			}
		}
	}

	// Tools mapping: Gemini functionDeclarations -> Codex tools
	tools := root.Get("tools")
	if tools.IsArray() {
		out, _ = sjson.SetRaw(out, "tools", `[]`)
		out, _ = sjson.Set(out, "tool_choice", "auto")
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			td := tarr[i]
			fns := td.Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			farr := fns.Array()
			for j := 0; j < len(farr); j++ {
				fn := farr[j]
				tool := `{}`
				tool, _ = sjson.Set(tool, "type", "function")
				if v := fn.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := shortMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					tool, _ = sjson.Set(tool, "name", name)
				}
				if v := fn.Get("description"); v.Exists() {
					tool, _ = sjson.Set(tool, "description", v.String())
				}
				if prm := fn.Get("parameters"); prm.Exists() {
					// Remove optional $schema field if present
					cleaned := prm.Raw
					cleaned, _ = sjson.Delete(cleaned, "$schema")
					cleaned, _ = sjson.Set(cleaned, "additionalProperties", false)
					tool, _ = sjson.SetRaw(tool, "parameters", cleaned)
				} else if prm = fn.Get("parametersJsonSchema"); prm.Exists() {
					// Remove optional $schema field if present
					cleaned := prm.Raw
					cleaned, _ = sjson.Delete(cleaned, "$schema")
					cleaned, _ = sjson.Set(cleaned, "additionalProperties", false)
					tool, _ = sjson.SetRaw(tool, "parameters", cleaned)
				}
				tool, _ = sjson.Set(tool, "strict", false)
				out, _ = sjson.SetRaw(out, "tools.-1", tool)
			}
		}
	}

	// Fixed flags aligning with Codex expectations
	out, _ = sjson.Set(out, "parallel_tool_calls", true)

	// Convert Gemini thinkingConfig to Codex reasoning.effort.
	// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
	effortSet := false
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.Set(out, "reasoning.effort", effort)
					effortSet = true
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.Set(out, "reasoning.effort", effort)
						effortSet = true
					}
				}
			}
		}
	}
	if !effortSet {
		// No thinking config, set default effort
		out, _ = sjson.Set(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.Set(out, "reasoning.summary", "auto")
	out, _ = sjson.Set(out, "stream", true)
	out, _ = sjson.Set(out, "store", false)
	out, _ = sjson.Set(out, "include", []string{"reasoning.encrypted_content"})

	var pathsToLower []string
	toolsResult := gjson.Get(out, "tools")
	util.Walk(toolsResult, "", "type", &pathsToLower)
	for _, p := range pathsToLower {
		fullPath := fmt.Sprintf("tools.%s", p)
		out, _ = sjson.Set(out, fullPath, strings.ToLower(gjson.Get(out, fullPath).String()))
	}

	return translatorcommon.OutputFormatFromGemini(root).WriteResponses([]byte(out))
}
