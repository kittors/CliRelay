// Package thinking provides unified thinking configuration processing.
//
// This file implements suffix parsing functionality for extracting
// thinking configuration from model names in the format model(value).
package thinking

import (
	"strconv"
	"strings"
)

// isContextWindowSuffix checks whether the content is a known context-window
// marker suffix (digits optionally followed by k/m for KB/MB scale).
// Valid examples: "1M", "128K", "32k", "4096". Invalid: "beta", "preview", "".
func isContextWindowSuffix(content string) bool {
	if content == "" {
		return false
	}
	// Must start with at least one digit
	i := 0
	for i < len(content) && content[i] >= '0' && content[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	// Optionally followed by a k/m scale suffix (case-insensitive)
	if i < len(content) {
		switch content[i] {
		case 'k', 'K', 'm', 'M':
			i++
		default:
			return false
		}
	}
	return i == len(content)
}

// StripBracketSuffix strips trailing [...] content from a model name
// when the content matches a known context-window marker pattern.
//
// Some clients append context window markers (e.g., "[1M]", "[128K]") to the
// model name. These markers should be stripped before model identification
// and are never treated as thinking suffixes. Non-matching bracket content
// (e.g., "[beta]", "[preview]") is preserved to avoid corrupting valid
// custom or provider model IDs.
//
// Examples:
//   - "deepseek-v4-flash[1M]" -> "deepseek-v4-flash"
//   - "model[128K]" -> "model"
//   - "model[beta]" -> "model[beta]" (unchanged, not a known marker)
//   - "model" -> "model" (unchanged)
//   - "model[1M](high)" -> "model(high)" (only trailing bracket stripped)
func StripBracketSuffix(model string) string {
	lastOpen := strings.LastIndex(model, "[")
	if lastOpen == -1 {
		return model
	}
	if !strings.HasSuffix(model, "]") {
		return model
	}
	// Only strip if there's content between brackets
	if lastOpen+1 >= len(model)-1 {
		return model
	}
	// Only strip known context-window markers to avoid corrupting
	// valid model IDs that legitimately end with brackets
	content := model[lastOpen+1 : len(model)-1]
	if !isContextWindowSuffix(content) {
		return model
	}
	return model[:lastOpen]
}

// ParseSuffix extracts thinking suffix from a model name.
//
// The suffix format is: model-name(value)
// Examples:
//   - "claude-sonnet-4-5(16384)" -> ModelName="claude-sonnet-4-5", RawSuffix="16384"
//   - "gpt-5.2(high)" -> ModelName="gpt-5.2", RawSuffix="high"
//   - "gemini-2.5-pro" -> ModelName="gemini-2.5-pro", HasSuffix=false
//   - "deepseek-v4-flash[1M]" -> ModelName="deepseek-v4-flash", HasSuffix=false
//   - "model[128K](8192)" -> ModelName="model", RawSuffix="8192"
//   - "model[1M](8192)" -> ModelName="model", RawSuffix="8192" (two-pass strip)
//
// Trailing [...] context window markers (digits + optional k/m) are stripped
// before and after round bracket parsing. Non-marker bracket content is
// preserved as part of the model name.
func ParseSuffix(model string) SuffixResult {
	// Strip trailing [...] bracket suffix (e.g., "[1M]", "[128K]") before
	// looking for round bracket thinking suffix. These are context window
	// markers appended by some clients and must not affect model identification.
	model = StripBracketSuffix(model)

	// Find the last opening parenthesis
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 {
		return SuffixResult{ModelName: model, HasSuffix: false}
	}

	// Check if the string ends with a closing parenthesis
	if !strings.HasSuffix(model, ")") {
		return SuffixResult{ModelName: model, HasSuffix: false}
	}

	// Extract components
	modelName := model[:lastOpen]
	rawSuffix := model[lastOpen+1 : len(model)-1]

	// Strip trailing [...] from the extracted model name too, in case
	// a bracket marker precedes the thinking suffix (e.g., "model[1M](8192)").
	modelName = StripBracketSuffix(modelName)

	return SuffixResult{
		ModelName: modelName,
		HasSuffix: true,
		RawSuffix: rawSuffix,
	}
}

// HasThinkingSuffix reports whether the model suffix defines a thinking configuration.
func HasThinkingSuffix(model string) bool {
	result := ParseSuffix(model)
	return result.HasSuffix && hasThinkingConfig(parseSuffixToConfig(result.RawSuffix, "", result.ModelName))
}

// ParseNumericSuffix attempts to parse a raw suffix as a numeric budget value.
//
// This function parses the raw suffix content (from ParseSuffix.RawSuffix) as an integer.
// Only non-negative integers are considered valid numeric suffixes.
//
// Platform note: The budget value uses Go's int type, which is 32-bit on 32-bit
// systems and 64-bit on 64-bit systems. Values exceeding the platform's int range
// will return ok=false.
//
// Leading zeros are accepted: "08192" parses as 8192.
//
// Examples:
//   - "8192" -> budget=8192, ok=true
//   - "0" -> budget=0, ok=true (represents ModeNone)
//   - "08192" -> budget=8192, ok=true (leading zeros accepted)
//   - "-1" -> budget=0, ok=false (negative numbers are not valid numeric suffixes)
//   - "high" -> budget=0, ok=false (not a number)
//   - "9223372036854775808" -> budget=0, ok=false (overflow on 64-bit systems)
//
// For special handling of -1 as auto mode, use ParseSpecialSuffix instead.
func ParseNumericSuffix(rawSuffix string) (budget int, ok bool) {
	if rawSuffix == "" {
		return 0, false
	}

	value, err := strconv.Atoi(rawSuffix)
	if err != nil {
		return 0, false
	}

	// Negative numbers are not valid numeric suffixes
	// -1 should be handled by special value parsing as "auto"
	if value < 0 {
		return 0, false
	}

	return value, true
}

// ParseSpecialSuffix attempts to parse a raw suffix as a special thinking mode value.
//
// This function handles special strings that represent a change in thinking mode:
//   - "none" -> ModeNone (disables thinking)
//   - "auto" -> ModeAuto (automatic/dynamic thinking)
//   - "-1"   -> ModeAuto (numeric representation of auto mode)
//
// String values are case-insensitive.
func ParseSpecialSuffix(rawSuffix string) (mode ThinkingMode, ok bool) {
	if rawSuffix == "" {
		return ModeBudget, false
	}

	// Case-insensitive matching
	switch strings.ToLower(rawSuffix) {
	case "none":
		return ModeNone, true
	case "auto", "-1":
		return ModeAuto, true
	default:
		return ModeBudget, false
	}
}

// ParseLevelSuffix attempts to parse a raw suffix as a discrete thinking level.
//
// This function parses the raw suffix content (from ParseSuffix.RawSuffix) as a level.
// Only discrete effort levels are valid: minimal, low, medium, high, xhigh, max.
// Level matching is case-insensitive.
//
// Special values (none, auto) are NOT handled by this function; use ParseSpecialSuffix
// instead. This separation allows callers to prioritize special value handling.
//
// Examples:
//   - "high" -> level=LevelHigh, ok=true
//   - "HIGH" -> level=LevelHigh, ok=true (case insensitive)
//   - "medium" -> level=LevelMedium, ok=true
//   - "none" -> level="", ok=false (special value, use ParseSpecialSuffix)
//   - "auto" -> level="", ok=false (special value, use ParseSpecialSuffix)
//   - "8192" -> level="", ok=false (numeric, use ParseNumericSuffix)
//   - "max" -> level=LevelMax, ok=true
//   - "ultra" -> level="", ok=false (Codex client mode, not a wire effort)
func ParseLevelSuffix(rawSuffix string) (level ThinkingLevel, ok bool) {
	if rawSuffix == "" {
		return "", false
	}

	// Case-insensitive matching
	switch strings.ToLower(rawSuffix) {
	case "minimal":
		return LevelMinimal, true
	case "low":
		return LevelLow, true
	case "medium":
		return LevelMedium, true
	case "high":
		return LevelHigh, true
	case "xhigh":
		return LevelXHigh, true
	case "max":
		return LevelMax, true
	default:
		return "", false
	}
}
