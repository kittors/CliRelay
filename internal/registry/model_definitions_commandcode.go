package registry

// Command Code model definitions.
//
// Kept out of model_definitions_static_data.go because that file is frozen at its
// current size by the structure ratchet in scripts/check-backend-structure.py and
// may only shrink.
//
// This list is a fallback, not a gate. Command Code publishes the authoritative
// catalog at GET /provider/v1/models, which needs no credential, so an operator
// running ahead of this snapshot can name any newer model explicitly under
// commandcode-api-key[].models and it will route without touching this file.
// Regenerate the snapshot from that endpoint rather than hand-editing entries.

// GetCommandCodeModels returns the Command Code Provider API catalog snapshot.
func GetCommandCodeModels() []*ModelInfo {
	definitions := []struct {
		id            string
		displayName   string
		contextLength int
	}{
		{"claude-sonnet-5", "Claude Sonnet 5", 1000000},
		{"claude-sonnet-4-6", "Claude Sonnet 4.6", 1000000},
		{"claude-fable-5", "Claude Fable 5", 1000000},
		{"claude-opus-5-5", "Claude Opus 5.5", 1000000},
		{"claude-opus-5", "Claude Opus 5", 1000000},
		{"claude-opus-4-8", "Claude Opus 4.8", 1000000},
		{"claude-opus-4-7", "Claude Opus 4.7", 1000000},
		{"claude-haiku-4-5-20251001", "Claude Haiku 4.5", 200000},
		{"gpt-5.6-sol", "GPT-5.6 Sol", 1050000},
		{"gpt-5.6-terra", "GPT-5.6 Terra", 1050000},
		{"gpt-5.6-luna", "GPT-5.6 Luna", 1050000},
		{"gpt-5.5", "GPT-5.5", 200000},
		{"gpt-5.4", "GPT-5.4", 400000},
		{"gpt-5.3-codex", "GPT-5.3 Codex", 400000},
		{"gpt-5.4-mini", "GPT-5.4 Mini", 400000},
		{"deepseek/deepseek-v4-pro", "DeepSeek V4 Pro (latest)", 1000000},
		{"deepseek/deepseek-v4-flash", "DeepSeek V4 Flash (latest)", 1000000},
		{"moonshotai/Kimi-K3", "Kimi K3", 1000000},
		{"moonshotai/Kimi-K2.7-Code", "Kimi K2.7 Code", 256000},
		{"moonshotai/Kimi-K2.7-Code-Highspeed", "Kimi K2.7 Code HighSpeed", 262000},
		{"moonshotai/Kimi-K2.6", "Kimi K2.6", 256000},
		{"moonshotai/Kimi-K2.5", "Kimi K2.5", 256000},
		{"zai-org/GLM-5.3", "GLM-5.3", 1000000},
		{"zai-org/GLM-5.2", "GLM-5.2", 1000000},
		{"zai-org/GLM-5.2-Fast", "GLM-5.2 Fast", 1000000},
		{"zai-org/GLM-5.1", "GLM-5.1", 200000},
		{"zai-org/GLM-5", "GLM-5", 200000},
		{"MiniMaxAI/MiniMax-M3", "MiniMax M3", 1000000},
		{"MiniMaxAI/MiniMax-M2.7", "MiniMax M2.7", 200000},
		{"MiniMaxAI/MiniMax-M2.5", "MiniMax M2.5", 200000},
		{"xiaomi/mimo-v2.5-pro", "MiMo V2.5 Pro", 1000000},
		{"xiaomi/mimo-v2.5", "MiMo V2.5", 1000000},
		{"Qwen/Qwen3.8-Max", "Qwen 3.8 Max", 1000000},
		{"Qwen/Qwen3.7-Max", "Qwen 3.7 Max", 1000000},
		{"Qwen/Qwen3.7-Plus", "Qwen 3.7 Plus", 1000000},
		{"Qwen/Qwen3.7-Flash", "Qwen 3.7 Flash", 1000000},
		{"Qwen/Qwen3.6-Max-Preview", "Qwen 3.6 Max Preview", 200000},
		{"Qwen/Qwen3.6-Plus", "Qwen 3.6 Plus", 200000},
		{"stepfun/Step-3.7-Flash", "Step 3.7 Flash", 256000},
		{"stepfun/Step-3.5-Flash", "Step 3.5 Flash", 1000000},
		{"tencent/hy3-paid", "Tencent Hy3", 262144},
		{"google/gemini-3.7-flash", "Gemini 3.7 Flash", 1048576},
		{"google/gemini-3.6-flash", "Gemini 3.6 Flash", 1000000},
		{"google/gemini-3.5-flash", "Gemini 3.5 Flash", 1000000},
		{"google/gemini-3.5-flash-lite", "Gemini 3.5 Flash Lite", 1000000},
		{"google/gemini-3.1-flash-lite", "Gemini 3.1 Flash Lite", 1000000},
		{"sakana/fugu-ultra", "Fugu Ultra", 1000000},
		{"nvidia/nemotron-3-ultra-550b-a55b", "Nemotron 3 Ultra", 1000000},
		{"thinkingmachines/inkling", "Inkling", 256000},
		{"thinkingmachines/inkling-small", "Inkling Small", 1000000},
		{"poolside/laguna-s-2.1-free", "Laguna S 2.1", 256000},
		{"meta/muse-spark-1.1", "Muse Spark 1.1", 1048576},
		{"meta/muse-spark-1.2", "Muse Spark 1.2", 1048576},
		{"meta/muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor", 1048576},
		{"xai/grok-4.5", "Grok 4.5", 500000},
		{"xai/grok-4.6", "Grok 4.6", 500000},
	}

	models := make([]*ModelInfo, 0, len(definitions))
	for _, definition := range definitions {
		models = append(models, &ModelInfo{
			ID:            definition.id,
			Object:        "model",
			Created:       1787060518,
			OwnedBy:       "command-code",
			Type:          "commandcode",
			DisplayName:   definition.displayName,
			ContextLength: definition.contextLength,
		})
	}
	return models
}
