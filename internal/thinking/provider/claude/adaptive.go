package claude

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyAdaptive writes thinking for Claude models that only take adaptive
// thinking (Opus 4.7 and later). Depth goes in output_config.effort; the
// budget form, which these models answer with a 400, is never written.
//
// Expected output for a level:
//
//	{
//	  "thinking": {"type": "adaptive", "display": "summarized"},
//	  "output_config": {"effort": "high"}
//	}
func applyAdaptive(body []byte, config thinking.ThinkingConfig, support *registry.ThinkingSupport) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	switch config.Mode {
	case thinking.ModeLevel:
		return setEffort(setAdaptiveThinking(body), config.Level)
	case thinking.ModeAuto:
		// Adaptive thinking is the model deciding how much to think. An effort
		// the request already carries stays; otherwise the model default applies.
		return setAdaptiveThinking(body)
	case thinking.ModeNone:
		return turnThinkingOff(body, support)
	case thinking.ModeBudget:
		// ValidateConfig and the configured-model path turn budgets into levels
		// for these models; a caller that skipped both still must not send one.
		level, ok := thinking.ConvertBudgetToLevel(config.Budget)
		if !ok {
			return body
		}
		switch thinking.ThinkingLevel(level) {
		case thinking.LevelNone:
			return turnThinkingOff(body, support)
		case thinking.LevelAuto:
			return setAdaptiveThinking(body)
		case thinking.LevelMinimal:
			level = string(lowestEffort(support))
		}
		return setEffort(setAdaptiveThinking(body), thinking.ThinkingLevel(level))
	}
	return body
}

// setAdaptiveThinking sets thinking.type "adaptive". A request that is already
// adaptive keeps the fields it chose. Any other shape is replaced whole:
// budget_tokens is what these models reject, and "between_tools" takes no other
// field. display becomes "summarized" unless the request set one, because
// Opus 4.7 and later default to "omitted" and stream thinking blocks with empty
// text, while the budget form being replaced returned summaries that clients
// show as reasoning.
func setAdaptiveThinking(body []byte) []byte {
	if gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
		if !gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
			return body
		}
		result, _ := sjson.DeleteBytes(body, "thinking.budget_tokens")
		return result
	}

	display := gjson.GetBytes(body, "thinking.display").String()
	if display == "" {
		display = "summarized"
	}
	result, _ := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"adaptive"}`))
	result, _ = sjson.SetBytes(result, "thinking.display", display)
	return result
}

// setEffort writes output_config.effort, leaving the body untouched when it
// already carries that level.
func setEffort(body []byte, level thinking.ThinkingLevel) []byte {
	if level == "" || gjson.GetBytes(body, "output_config.effort").String() == string(level) {
		return body
	}
	result, _ := sjson.SetBytes(body, "output_config.effort", string(level))
	return result
}

// turnThinkingOff applies the model's own way of turning thinking off, which
// differs per model and is a 400 when wrong.
func turnThinkingOff(body []byte, support *registry.ThinkingSupport) []byte {
	adaptive := support.Adaptive
	if adaptive == nil || adaptive.Off == registry.ClaudeThinkingAlwaysOn {
		// Opus 5.5 and Fable reject "disabled" at every effort. Anthropic's
		// replacement is to omit thinking and lower the effort, so the request
		// gets the lowest one unless it names its own.
		result, _ := sjson.DeleteBytes(body, "thinking")
		if gjson.GetBytes(result, "output_config.effort").Exists() {
			return result
		}
		return setEffort(result, lowestEffort(support))
	}

	// Neither "disabled" nor "between_tools" takes any other field.
	result, _ := sjson.SetRawBytes(body, "thinking", []byte(`{}`))
	result, _ = sjson.SetBytes(result, "thinking.type", adaptive.Off)
	if ceiling := thinking.ThinkingLevel(adaptive.OffMaxLevel); ceiling != "" {
		effort := thinking.ThinkingLevel(gjson.GetBytes(result, "output_config.effort").String())
		if thinking.LevelAbove(effort, ceiling) {
			// Opus 5 and Sonnet 5.5 refuse to turn thinking off above "high".
			// Turning it off is what the request asked for, so the effort gives way.
			result = setEffort(result, ceiling)
		}
	}
	return result
}

// lowestEffort is the model's lowest listed effort level.
func lowestEffort(support *registry.ThinkingSupport) thinking.ThinkingLevel {
	if support != nil && len(support.Levels) > 0 {
		return thinking.ThinkingLevel(support.Levels[0])
	}
	return thinking.LevelLow
}
