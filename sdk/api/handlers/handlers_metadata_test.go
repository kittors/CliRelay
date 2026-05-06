package handlers

import (
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestEnrichRequestExecutionMetadataMarksTopLevelTools(t *testing.T) {
	meta := map[string]any{}
	raw := []byte(`{
		"model": "gpt-5.3-codex",
		"input": [{"role": "user", "content": "run tests"}],
		"tools": [
			{"type": "function", "name": "exec_command"},
			{"type": "function", "name": "apply_patch"}
		]
	}`)

	enrichRequestExecutionMetadata(meta, raw)

	if got := meta[coreexecutor.ToolDefinitionsMetadataKey]; got != 2 {
		t.Fatalf("tool definitions = %v, want 2", got)
	}
	features, ok := meta[coreexecutor.RequestFeaturesMetadataKey].([]string)
	if !ok {
		t.Fatalf("features metadata type = %T, want []string", meta[coreexecutor.RequestFeaturesMetadataKey])
	}
	for _, feature := range features {
		if feature == "tools" {
			return
		}
	}
	t.Fatalf("features = %v, want tools", features)
}
