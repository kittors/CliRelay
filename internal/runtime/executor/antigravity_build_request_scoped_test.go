package executor

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestAntigravitySchemaCleanupMatchesWholePayloadOnTranslatorRequests takes the
// random Gemini and Gemini CLI requests the translator tests use, through the
// steps buildRequest runs before the schema cleanup, and holds the scoped cleanup
// to the whole-payload code it replaced, byte for byte.
func TestAntigravitySchemaCleanupMatchesWholePayloadOnTranslatorRequests(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		raw := translatortest.GeminiRequest(r)
		if seed%2 == 0 {
			raw = translatortest.GeminiCLIRequest(r)
		}
		for _, model := range antigravityBuildGoldenModels {
			payload := geminiToAntigravity(model, raw, "p", 0)
			payload, _ = sjson.SetBytes(payload, "model", model)
			payloadStr := util.NormalizeGeminiContentRoles(string(payload), "request.contents")
			sanitized := stripAntigravityUnsupportedThinkingConfig(sanitizeAntigravityThoughts([]byte(payloadStr), model), model)
			payloadStr = string(sanitized)
			antigravitySchema := strings.Contains(model, "claude") || strings.Contains(model, "gemini-3-pro-high")

			want := wholePayloadSchemaCleanup(payloadStr, antigravitySchema)
			if got := util.CleanAntigravityRequestSchemas(payloadStr, antigravitySchema); got != want {
				t.Fatalf("seed %d, %s: scoped cleanup differs\n%s", seed, model, describeFirstDifference([]byte(want), []byte(got)))
			}
		}
	}
}

// wholePayloadSchemaCleanup is what buildRequest ran before the scoped cleanup.
func wholePayloadSchemaCleanup(payloadStr string, antigravitySchema bool) string {
	paths := make([]string, 0)
	util.Walk(gjson.Parse(payloadStr), "", "parametersJsonSchema", &paths)
	for _, p := range paths {
		payloadStr, _ = util.RenameKey(payloadStr, p, p[:len(p)-len("parametersJsonSchema")]+"parameters")
	}
	if antigravitySchema {
		return util.CleanJSONSchemaForAntigravity(payloadStr)
	}
	return util.CleanJSONSchemaForGemini(payloadStr)
}
