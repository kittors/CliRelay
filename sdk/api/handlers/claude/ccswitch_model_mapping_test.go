package claude

import (
	"testing"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/tidwall/gjson"
)

func TestRewriteCcSwitchClaudeRequestModelMapsConfiguredRequestModel(t *testing.T) {
	rawJSON := []byte(`{"model":"claude-opus-4-7","max_tokens":32,"messages":[{"role":"user","content":"ok"}]}`)
	route := &internalrouting.PathRouteContext{
		RoutePath: "/kimicode/cs_opus",
		Group:     "kimicode",
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ConfigID:   "cfg-kimi-opus",
			ClientType: "claude",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{Role: "opus", RequestModel: "claude-opus-4-7", TargetModel: "kimi-k2.6"},
			},
		},
	}

	rewritten, modelName, mapped := rewriteCcSwitchClaudeRequestModel(rawJSON, route)
	if !mapped {
		t.Fatal("mapped = false, want true")
	}
	if modelName != "kimi-k2.6" {
		t.Fatalf("modelName = %q, want kimi-k2.6", modelName)
	}
	if got := gjson.GetBytes(rewritten, "model").String(); got != "kimi-k2.6" {
		t.Fatalf("rewritten model = %q, want kimi-k2.6", got)
	}
	if got := gjson.GetBytes(rawJSON, "model").String(); got != "claude-opus-4-7" {
		t.Fatalf("input rawJSON mutated, model = %q", got)
	}
}

func TestRewriteCcSwitchClaudeRequestModelLeavesUnmappedModelUnchanged(t *testing.T) {
	rawJSON := []byte(`{"model":"claude-sonnet-4-5","max_tokens":32}`)
	route := &internalrouting.PathRouteContext{
		RoutePath: "/kimicode/cs_opus",
		Group:     "kimicode",
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ConfigID:   "cfg-kimi-opus",
			ClientType: "claude",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{Role: "opus", RequestModel: "claude-opus-4-7", TargetModel: "kimi-k2.6"},
			},
		},
	}

	rewritten, modelName, mapped := rewriteCcSwitchClaudeRequestModel(rawJSON, route)
	if mapped {
		t.Fatal("mapped = true, want false")
	}
	if modelName != "claude-sonnet-4-5" {
		t.Fatalf("modelName = %q, want original model", modelName)
	}
	if string(rewritten) != string(rawJSON) {
		t.Fatalf("rewritten JSON changed: %s", rewritten)
	}
}
