package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCleanJSONSchemaForGemini_FillsMissingArrayItems(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "bare array property",
			input:    `{"type":"object","properties":{"machineIds":{"type":"array","description":"Target machines"}}}`,
			expected: `{"type":"object","properties":{"machineIds":{"type":"array","description":"Target machines","items":{}}}}`,
		},
		{
			name:     "declared items are kept",
			input:    `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`,
			expected: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`,
		},
		{
			name:     "uppercase ARRAY from native Gemini requests",
			input:    `{"type":"OBJECT","properties":{"ids":{"type":"ARRAY"}}}`,
			expected: `{"type":"OBJECT","properties":{"ids":{"type":"ARRAY","items":{}}}}`,
		},
		{
			name:     "array nested in items",
			input:    `{"type":"array","items":{"type":"array"}}`,
			expected: `{"type":"array","items":{"type":"array","items":{}}}`,
		},
		{
			name:     "root array",
			input:    `{"type":"array"}`,
			expected: `{"type":"array","items":{}}`,
		},
		{
			name:     "property named type is not the keyword",
			input:    `{"type":"object","properties":{"type":{"type":"string"}}}`,
			expected: `{"type":"object","properties":{"type":{"type":"string"}}}`,
		},
		{
			// Malformed, but a property name must never be read as the keyword.
			name:     "property named type holding a bare string",
			input:    `{"type":"object","properties":{"type":"array"}}`,
			expected: `{"type":"object","properties":{"type":"array"}}`,
		},
		{
			name:     "property named items gets its own items",
			input:    `{"type":"object","properties":{"items":{"type":"array"}}}`,
			expected: `{"type":"object","properties":{"items":{"type":"array","items":{}}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareJSON(t, tc.expected, CleanJSONSchemaForGemini(tc.input))
		})
	}
}

// Arrays that only become plain "array" during cleanup must still get items.
func TestCleanJSONSchemaForGemini_FillsItemsOnFlattenedArrays(t *testing.T) {
	cases := map[string]string{
		"nullable type list": `{"type":"object","properties":{"ids":{"type":["array","null"]}}}`,
		"anyOf branch":       `{"type":"object","properties":{"ids":{"anyOf":[{"type":"array"},{"type":"null"}]}}}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out := CleanJSONSchemaForGemini(input)
			if got := gjson.Get(out, "properties.ids.type").String(); got != "array" {
				t.Fatalf("type = %q, want array; schema: %s", got, out)
			}
			if items := gjson.Get(out, "properties.ids.items"); !items.IsObject() {
				t.Fatalf("items missing; schema: %s", out)
			}
		})
	}
}

func TestCleanJSONSchemaForAntigravity_FillsMissingArrayItems(t *testing.T) {
	out := CleanJSONSchemaForAntigravity(`{"type":"object","properties":{"ids":{"type":"array"}},"required":["ids"]}`)
	items := gjson.Get(out, "properties.ids.items")
	// The placeholder pass fills empty object schemas; {} has no type and must stay empty.
	if !items.IsObject() || len(items.Map()) != 0 {
		t.Fatalf("items = %s, want {}; schema: %s", items.Raw, out)
	}
}

// The executor cleans the whole payload, history included. A replayed function call is
// data, even when its arguments look like a schema.
func TestCleanJSONSchemaForGemini_LeavesReplayedCallArgumentsAlone(t *testing.T) {
	cases := map[string]string{
		"antigravity envelope": "request.",
		"native gemini body":   "",
	}
	for name, prefix := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"define_column","args":{"name":"tags","type":"array"}}}]}],` +
				`"tools":[{"functionDeclarations":[{"name":"define_column","parameters":{"type":"object","properties":{"ids":{"type":"array"}}}}]}]}`
			if prefix != "" {
				body = `{"request":` + body + `}`
			}
			out := CleanJSONSchemaForGemini(body)
			if !gjson.Get(out, prefix+"tools.0.functionDeclarations.0.parameters.properties.ids.items").IsObject() {
				t.Fatalf("tool schema array not filled: %s", out)
			}
			if gjson.Get(out, prefix+"contents.0.parts.0.functionCall.args.items").Exists() {
				t.Fatalf("replayed call arguments were rewritten: %s", out)
			}
		})
	}
}
