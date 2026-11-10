package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// The data of a tool call or result is the caller's: a key there named like
// one of the protocol's ("id", "type", "signature") does not keep its value
// from being masked.
func TestMaskingReachesToolDataUnderAnyKey(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	for _, field := range []string{"command", "id", "type", "status", "signature", "encrypted_content", "call_id", "tool_use_id", "cache_control"} {
		t.Run(field, func(t *testing.T) {
			value, _ := json.Marshal(map[string]string{field: testOpenAIKey})
			body := []byte(`{"model":"claude","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":` + string(value) + `}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"go"}]}]}`)
			req, opts := guardOptions("tenant-a", body)
			opts.SourceFormat = sdktranslator.FormatClaude
			result := moderator.GuardRequest(context.Background(), req, opts)
			if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestMaskingReachesToolDataAcrossFormats(t *testing.T) {
	// "type":"image" in the caller's data is not a media block
	data := `{"id":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `","type":"image","content":"` + testOpenAIKey + `"}`
	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		body   string
	}{
		{"chat tool call", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":` + quoteJSON(data) + `}}]}]}`},
		{"chat legacy function call", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","function_call":{"name":"run","arguments":` + quoteJSON(data) + `}}]}`},
		{"responses function call", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call","call_id":"c1","name":"run","arguments":` + quoteJSON(data) + `}]}`},
		{"responses mcp call", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"mcp_call","id":"m1","name":"run","arguments":` + quoteJSON(data) + `}]}`},
		{"claude tool use", sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":` + data + `}]}]}`},
		{"gemini function call", sdktranslator.FormatGemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"run","args":` + data + `}}]}]}`},
		{"gemini function response", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"run","response":` + data + `}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masker := testMasker(6)
			masked, failure := maskRequestSecrets(tc.format, []byte(tc.body), masker, false)
			if failure != "" || masker.Places() != 3 || bytes.Contains(masked, []byte(testOpenAIKey)) || !gjson.ValidBytes(masked) {
				t.Fatalf("places = %d, failure = %q, masked = %s", masker.Places(), failure, masked)
			}
			// only the values changed: putting them back gives the request byte for byte
			if got := masker.Restorer().Restore(string(masked), false); got != tc.body {
				t.Fatalf("restored request differs:\n%s\n%s", got, tc.body)
			}
		})
	}
}

// What was read before is read as before: a tool result that is a string is
// one text, and inline media in a tool's data is not text.
func TestMaskingReadsToolResultsAndMediaAsBefore(t *testing.T) {
	data := `{"id":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `","type":"image","content":"` + testOpenAIKey + `"}`
	for _, tc := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + quoteJSON(data) + `}]}]}`},
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"tool","tool_call_id":"t1","content":` + quoteJSON(data) + `}]}`},
		{sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call_output","call_id":"t1","output":` + quoteJSON(data) + `}]}`},
	} {
		masker := testMasker(7)
		masked, failure := maskRequestSecrets(tc.format, []byte(tc.body), masker, false)
		if failure != "" || masker.Places() != 3 || bytes.Contains(masked, []byte(testOpenAIKey)) {
			t.Fatalf("format %v: places = %d, failure = %q", tc.format, masker.Places(), failure)
		}
	}
	media := `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"shot","response":{"image":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"}}}]}]}`
	masker := testMasker(7)
	if masked, failure := maskRequestSecrets(sdktranslator.FormatGemini, []byte(media), masker, false); masked != nil || failure != "" || masker.Places() != 0 {
		t.Fatalf("inline media was read: places = %d, failure = %q", masker.Places(), failure)
	}
}

// The ids, types and statuses tools return every day are not secrets.
func TestMaskingDoesNotTakeOrdinaryToolDataForSecrets(t *testing.T) {
	data := `{"id":"toolu_01A09q90qw90lq917835lq9","type":"image","status":"completed","call_id":"call_abc123","uuid":"3f2b8a9c-1d4e-4b7a-9c2f-5e6d7a8b9c0d","signature":"approved","items":[{"id":42,"type":"file","name":"main.go"}]}`
	body := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":` + data + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + quoteJSON(data) + `}]}]}`
	masker := testMasker(8)
	if masked, failure := maskRequestSecrets(sdktranslator.FormatClaude, []byte(body), masker, false); masked != nil || failure != "" {
		t.Fatalf("ordinary tool data was masked: %+v, failure = %q", masker.Values(), failure)
	}
}

func TestMaskingReachesToolDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		body   string
		places int
	}{
		{"chat tools and nested schema", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"run","description":"Use ` + testOpenAIKey + ` to sign in",` +
			`"parameters":{"type":"object","properties":{"token":{"type":"string","description":"defaults to ` + testOpenAIKey + `"},"description":{"type":"object","properties":{"deep":{"type":"string","description":"` + testOpenAIKey + `"}}}}}}}]}`, 3},
		{"chat legacy functions", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"go"}],"functions":[{"name":"run","description":"Use ` + testOpenAIKey + `","parameters":{"type":"object"}}]}`, 1},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"input":"go","tools":[{"type":"function","name":"run","description":"Use ` + testOpenAIKey + `","parameters":{"type":"object"}}]}`, 1},
		{"claude", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"go"}],"tools":[{"name":"run","description":"Use ` + testOpenAIKey + `","input_schema":{"type":"object","properties":{"k":{"type":"string","description":"` + testOpenAIKey + `"}}}}]}`, 2},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"go"}]}],"tools":[{"functionDeclarations":[{"name":"run","description":"Use ` + testOpenAIKey + `","parameters":{"type":"OBJECT","properties":{"k":{"type":"STRING","description":"` + testOpenAIKey + `"}}}}]}]}`, 2},
		{"gemini cli", sdktranslator.FormatGeminiCLI, `{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"go"}]}],"tools":[{"functionDeclarations":[{"name":"run","description":"Use ` + testOpenAIKey + `"}]}]}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masker := testMasker(9)
			masked, failure := maskRequestSecrets(tc.format, []byte(tc.body), masker, false)
			if failure != "" || masker.Places() != tc.places || bytes.Contains(masked, []byte(testOpenAIKey)) || !gjson.ValidBytes(masked) {
				t.Fatalf("places = %d (want %d), failure = %q, masked = %s", masker.Places(), tc.places, failure, masked)
			}
			// names and schemas are left as they were
			if got := masker.Restorer().Restore(string(masked), false); got != tc.body {
				t.Fatalf("restored request differs:\n%s\n%s", got, tc.body)
			}
		})
	}
	// and through the guard, with the reply filter that puts the key back
	moderator := maskingModerator(t, "tenant-a")
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"run","description":"Use ` + testOpenAIKey + ` to authenticate","parameters":{"type":"object","properties":{}}}}]}`)
	req, opts := guardOptions("tenant-a", body)
	result := moderator.GuardRequest(context.Background(), req, opts)
	if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) || result.Reply == nil {
		t.Fatalf("result = %+v", result)
	}
}

// The word guard reads what it read before: tools are not its, and in tool
// data the values under the protocol's keys stay out of what it can rewrite.
func TestMaskingCoverageLeavesTheWordGuardAsItWas(t *testing.T) {
	for _, tc := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"run","description":"` + testOpenAIKey + `","parameters":{"type":"object","properties":{"arg":{"type":"string","description":"` + testOpenAIKey + `"}}}}}]}`},
		{sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"run","description":"` + testOpenAIKey + `","input_schema":{"type":"object"}}]}`},
		{sdktranslator.FormatGeminiCLI, `{"request":{"contents":[],"tools":[{"functionDeclarations":[{"name":"run","description":"` + testOpenAIKey + `"}]}]}}`},
		{sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":{"id":"` + testOpenAIKey + `"}}]}]}`},
	} {
		for _, unit := range extractWordUnits(tc.format, []byte(tc.body)) {
			if strings.Contains(unit.text, testOpenAIKey) {
				t.Fatalf("the word guard now reads %q of %s", unit.text, tc.body)
			}
		}
		// while masking does read it
		found := false
		for _, unit := range extractSecretUnits(tc.format, []byte(tc.body)) {
			found = found || (strings.Contains(unit.text, testOpenAIKey) && unit.rewritable)
		}
		if !found {
			t.Fatalf("masking does not read the key of %s", tc.body)
		}
	}
}

// The request's own protocol and text a signature covers stay as they came.
func TestMaskingKeepsTheProtocolAndSignedText(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `"},` +
		`{"type":"tool_use","id":"` + testOpenAIKey + `","name":"run","input":{"id":"` + testOpenAIKey + `"}}]}]}`)
	masker := testMasker(4)
	masked, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, masker, false)
	if failure != "" || masker.Places() != 1 {
		t.Fatalf("places = %d, failure = %q", masker.Places(), failure)
	}
	for _, path := range []string{"messages.0.content.0.thinking", "messages.0.content.0.signature", "messages.0.content.1.id"} {
		if gjson.GetBytes(masked, path).String() != testOpenAIKey {
			t.Fatalf("%s was changed", path)
		}
	}
	if gjson.GetBytes(masked, "messages.0.content.1.input.id").String() == testOpenAIKey {
		t.Fatal("the id in the tool's input was not masked")
	}
	gemini := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"` + testOpenAIKey + `","thoughtSignature":"c2ln"},{"text":"` + testOpenAIKey + `"}]}]}`)
	masker = testMasker(5)
	masked, failure = maskRequestSecrets(sdktranslator.FormatGemini, gemini, masker, false)
	if failure != "" || masker.Places() != 1 || gjson.GetBytes(masked, "contents.0.parts.0.text").String() != testOpenAIKey || gjson.GetBytes(masked, "contents.0.parts.0.thoughtSignature").String() != "c2ln" {
		t.Fatalf("a Gemini thought was changed: places = %d, failure = %q", masker.Places(), failure)
	}
}
