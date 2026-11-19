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

// TestAntigravitySchemaCleanupChangesOnlyContentsOnTranslatorRequests takes the
// random Gemini and Gemini CLI requests the translator tests use, through the
// steps buildRequest runs before the schema cleanup, and holds the cleanup to the
// whole-payload code it replaced, byte for byte, everywhere but in
// request.contents: that code cleaned the calls replayed there as if they were
// schemas, and the cleanup now leaves them as they are. These requests put schemas
// in every place the translators do, so a place the cleanup failed to look would
// show up here as a schema left uncleaned.
func TestAntigravitySchemaCleanupChangesOnlyContentsOnTranslatorRequests(t *testing.T) {
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

			got := util.CleanAntigravityRequestSchemas(payloadStr, antigravitySchema)
			contents := gjson.Get(payloadStr, "request.contents")
			if gjson.Get(got, "request.contents").Raw != contents.Raw {
				t.Fatalf("seed %d, %s: contents changed\nwant: %s\ngot:  %s", seed, model, contents.Raw, gjson.Get(got, "request.contents").Raw)
			}
			want := wholePayloadSchemaCleanup(payloadStr, antigravitySchema)
			if contents.Exists() {
				want, _ = sjson.SetRaw(want, "request.contents", contents.Raw)
			}
			if got != want {
				t.Fatalf("seed %d, %s: differs from the whole-payload cleanup outside contents\n%s", seed, model, describeFirstDifference([]byte(want), []byte(got)))
			}
		}
	}
}

// wholePayloadSchemaCleanup is what buildRequest ran before the cleanup was
// confined to the schemas a request declares.
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
