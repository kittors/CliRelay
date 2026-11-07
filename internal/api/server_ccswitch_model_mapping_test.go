package api

import (
	"testing"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/tidwall/gjson"
)

func TestCcSwitchOpenAIModelMappingUsesCodexMappings(t *testing.T) {
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType: "codex",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
			},
		},
	}

	mapping := ccSwitchOpenAIModelMapping(route)
	if mapping["deepseek-v4-flash"] != "deepseek-chat" {
		t.Fatalf("mapping = %#v, want deepseek-v4-flash -> deepseek-chat", mapping)
	}
}

func TestRewriteCcSwitchOpenAIModelMapsRequestModel(t *testing.T) {
	rawJSON := []byte(`{"model":"deepseek-v4-flash","input":"ok"}`)

	rewritten, modelName, mapped := rewriteCcSwitchOpenAIModel(rawJSON, map[string]string{
		"deepseek-v4-flash": "deepseek-chat",
	})
	if !mapped {
		t.Fatal("mapped = false, want true")
	}
	if modelName != "deepseek-chat" {
		t.Fatalf("modelName = %q, want deepseek-chat", modelName)
	}
	if got := gjson.GetBytes(rewritten, "model").String(); got != "deepseek-chat" {
		t.Fatalf("rewritten model = %q, want deepseek-chat", got)
	}
}

func TestCcSwitchOpenAIModelMappingCoversOpenAIProtocolApps(t *testing.T) {
	mappings := []internalrouting.CcSwitchModelMapping{
		{RequestModel: "gpt-5.4", TargetModel: "deepseek-v4-flash"},
	}
	for _, clientType := range []string{"codex", "opencode", "openclaw", "hermes", "OpenCode"} {
		route := &internalrouting.PathRouteContext{
			CcSwitch: &internalrouting.CcSwitchRouteContext{ClientType: clientType, ModelMappings: mappings},
		}
		if got := ccSwitchOpenAIModelMapping(route)["gpt-5.4"]; got != "deepseek-v4-flash" {
			t.Errorf("%s mapping[gpt-5.4] = %q, want deepseek-v4-flash", clientType, got)
		}
	}
	// Claude Code is rewritten in the Claude handler and Gemini CLI carries the
	// model in the URL path; neither belongs to the OpenAI body rewrite.
	for _, clientType := range []string{"claude", "gemini", ""} {
		route := &internalrouting.PathRouteContext{
			CcSwitch: &internalrouting.CcSwitchRouteContext{ClientType: clientType, ModelMappings: mappings},
		}
		if mapping := ccSwitchOpenAIModelMapping(route); mapping != nil {
			t.Errorf("%q mapping = %#v, want nil", clientType, mapping)
		}
	}
}
