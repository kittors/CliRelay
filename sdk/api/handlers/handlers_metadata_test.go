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

func TestEnrichRequestExecutionMetadataDistinguishesMediaFeatures(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantHas string
	}{
		{
			name:    "image",
			raw:     `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/x.png"}]}]}`,
			wantHas: "image",
		},
		{
			name:    "file",
			raw:     `{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://example.com/x.txt"}]}]}`,
			wantHas: "file",
		},
		{
			name:    "video",
			raw:     `{"input":[{"role":"user","content":[{"type":"input_video","video_url":"https://example.com/x.mp4"}]}]}`,
			wantHas: "video",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := map[string]any{}
			enrichRequestExecutionMetadata(meta, []byte(tt.raw))
			features, ok := meta[coreexecutor.RequestFeaturesMetadataKey].([]string)
			if !ok {
				t.Fatalf("features metadata type = %T, want []string", meta[coreexecutor.RequestFeaturesMetadataKey])
			}
			foundMultimodal := false
			foundWanted := false
			for _, feature := range features {
				if feature == "multimodal" {
					foundMultimodal = true
				}
				if feature == tt.wantHas {
					foundWanted = true
				}
			}
			if !foundMultimodal || !foundWanted {
				t.Fatalf("features = %v, want multimodal and %s", features, tt.wantHas)
			}
		})
	}
}
