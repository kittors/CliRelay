package contentmoderation

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// codexScreenshotRequest is a Chat Completions request as Codex sends one late
// in a session: the whole conversation again, a screenshot in every user turn,
// and calls shell calls and their output after it. Most of its bytes are
// base64, which masking does not read but every walk of the request crosses.
// It returns the request and the values planted in it.
//
// Each call passes a key in a header and its output prints a .env, and the
// model repeats the key at the end of the turn: two strings with values to
// mask per call, and one per turn. With distinct, every call's values are new
// (a key per environment, a token per run); otherwise every call shows the
// same ones again, as a session that reads one .env does.
func codexScreenshotRequest(turns, calls, imageBytes int, distinct bool) ([]byte, []string) {
	rng := rand.New(rand.NewSource(1))
	token := func(prefix string, n int) string {
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return prefix + string(b)
	}
	image := make([]byte, imageBytes*3/4)
	rng.Read(image)
	screenshot := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)

	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5-codex","stream":true,"tools":[{"type":"function","function":{"name":"shell",` +
		`"description":"Runs a shell command in the workspace and returns what it printed.",` +
		`"parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"description":"The command and its arguments."},` +
		`"workdir":{"type":"string","description":"Where to run it."}},"required":["command"]}}}],"messages":[`)
	b.WriteString(`{"role":"system","content":` + quoteJSON(strings.Repeat("You are a coding agent working in the user's repository. Keep changes small. ", 80)) + `}`)
	openAIKey, githubToken, password := testOpenAIKey, token("ghp_", 36), "Tr0ub4dor3x"
	secrets := []string{openAIKey, githubToken, password}
	logLines := strings.Repeat("2026-11-10T19:56:02.118Z INFO request_id=7f3a2b1c path=/v1/chat/completions status=200 latency_ms=812\n", 12)
	for turn := 0; turn < turns; turn++ {
		fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"text","text":%s},{"type":"image_url","image_url":{"url":%s}}]}`,
			quoteJSON(fmt.Sprintf("Turn %d: the page still shows the old build, see the screenshot. Check the deploy.", turn)), quoteJSON(screenshot))
		for call := 0; call < calls; call++ {
			if distinct {
				openAIKey, githubToken, password = token("sk-proj-", 48), token("ghp_", 36), token("Pw", 10)
				secrets = append(secrets, openAIKey, githubToken, password)
			}
			id := turn*calls + call
			arguments := `{"command":["bash","-lc","curl -sS -H 'Authorization: Bearer ` + openAIKey + `' https://api.openai.com/v1/models | head"],"workdir":"/repo"}`
			output := logLines + "$ cat .env\nOPENAI_API_KEY=" + openAIKey + "\nGITHUB_TOKEN=" + githubToken +
				"\nDATABASE_URL=postgres://app:" + password + "@db.internal:5432/app\n" + logLines
			fmt.Fprintf(&b, `,{"role":"assistant","content":null,"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"shell","arguments":%s}}]}`,
				id, quoteJSON(arguments))
			fmt.Fprintf(&b, `,{"role":"tool","tool_call_id":"call_%d","content":%s}`, id, quoteJSON(output))
		}
		fmt.Fprintf(&b, `,{"role":"assistant","content":%s}`,
			quoteJSON("The .env sets OPENAI_API_KEY to "+openAIKey+" and the models call works with it; the deploy is next."))
	}
	b.WriteString(`]}`)
	if distinct {
		secrets = secrets[3:] // the first ones were replaced before any was written
	}
	return []byte(b.String()), secrets
}

// BenchmarkMaskRequestCodexScreenshots masks the first copy of a request (the
// one the request's record notes) of about 10 MB, as the privacy-masking guard
// does on every turn of a Codex session. The scope has seen the request
// before, as it has on every turn after the first: what the rules find is
// remembered, and what is left is reading the request and writing it.
//
// The cost of masking moves with what a request holds as much as with its
// size (two profiles of production an evening apart differ sevenfold), so the
// last case is the worst one met: a dozen calls a turn, each with new values.
func BenchmarkMaskRequestCodexScreenshots(b *testing.B) {
	for _, c := range []struct {
		name     string
		calls    int
		distinct bool
	}{
		{"same-secrets-every-turn", 1, false},
		{"new-secrets-every-turn", 1, true},
		{"12-calls-a-turn-new-secrets", 12, true},
	} {
		body, secrets := codexScreenshotRequest(18, c.calls, 540<<10, c.distinct)
		b.Run(c.name, func(b *testing.B) {
			scope := privacymask.NewScopes().Get("bench")
			key := bytes.Repeat([]byte{7}, secretMaskingKeyBytes)
			mask := func() ([]byte, *privacymask.Masker, *maskNotes) {
				masker := privacymask.NewMasker(key, privacymask.Options{Secrets: true}, scope, "gpt-5.5-codex")
				notes := newMaskNotes()
				masked, failure := maskRequest(sdktranslator.FormatOpenAI, body, masker, false, notes)
				if failure != "" || masked == nil {
					b.Fatalf("masking failed: %q", failure)
				}
				return masked, masker, notes
			}
			masked, masker, notes := mask()
			if !gjson.ValidBytes(masked) || len(masker.Values()) == 0 {
				b.Fatalf("the request was not masked: %d values", len(masker.Values()))
			}
			for _, secret := range secrets {
				if bytes.Contains(masked, []byte(secret)) {
					b.Fatalf("%.12s… reached the upstream", secret)
				}
			}
			for _, v := range masker.Values() {
				if notes.fields[v.ID] == "" {
					b.Fatalf("value %s has no field", v.ID)
				}
			}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				mask()
			}
			b.ReportMetric(float64(len(masker.Values())), "values")
		})
	}
}
