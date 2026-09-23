package responses

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

// TestConvertOpenAIResponsesRequestToGemini_NullableToolSchemaTypes covers JSON Schema 2020-12
// union types in tool declarations. Before the fix these reached Gemini as the raw array text
// (e.g. "[\"BOOLEAN\",\"NULL\"]"), which is not a Type enum member and fails the request with
// INVALID_ARGUMENT (400).
func TestConvertOpenAIResponsesRequestToGemini_NullableToolSchemaTypes(t *testing.T) {
	req := []byte(`{
		"model": "gemini-3.8-flash-tiered",
		"input": "Reply with exactly: OK",
		"tools": [{
			"type": "function",
			"name": "browser_click",
			"description": "click an element",
			"parameters": {
				"type": "object",
				"properties": {
					"pageId":   {"type": "number"},
					"dblClick": {"type": ["boolean", "null"]},
					"uid":      {"type": ["string", "null"]},
					"onlyNull": {"type": ["null"]}
				},
				"required": ["pageId", "dblClick"]
			}
		}]
	}`)

	out := string(ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", req, false))
	properties := "tools.0.functionDeclarations.0.parametersJsonSchema.properties."

	for _, tc := range []struct {
		property string
		want     string
	}{
		{"pageId", "NUMBER"},
		{"dblClick", "BOOLEAN"},
		{"uid", "STRING"},
		{"onlyNull", "STRING"},
	} {
		if got := gjson.Get(out, properties+tc.property+".type").String(); got != tc.want {
			t.Errorf("property %q: expected type %q, got %q", tc.property, tc.want, got)
		}
	}

	// Every emitted type must survive both schema cleaners unchanged; a stringified union
	// slips past flattenTypeArrays because it is no longer an array by the time it runs.
	schema := gjson.Get(out, "tools.0.functionDeclarations.0.parametersJsonSchema").Raw
	for name, cleaned := range map[string]string{
		"gemini":      util.CleanJSONSchemaForGemini(schema),
		"antigravity": util.CleanJSONSchemaForAntigravity(schema),
	} {
		gjson.Get(cleaned, "properties").ForEach(func(key, value gjson.Result) bool {
			propType := value.Get("type").String()
			if strings.ContainsAny(propType, "[]\"") {
				t.Errorf("%s cleaner: property %q kept a non-enum type %q", name, key.String(), propType)
			}
			return true
		})
	}
}
