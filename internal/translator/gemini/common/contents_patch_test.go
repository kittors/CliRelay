package common

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

// Each patch must produce what the per-path edit it replaced produces, on
// every random Gemini and Gemini CLI request. The translators' own
// equivalence tests compare whole requests; this one also runs the per-path
// code the patches fall back to, which those requests rarely reach.
func TestPatchesMatchPerPathEdits(t *testing.T) {
	r := rand.New(rand.NewSource(20261116))
	edits := []struct {
		name       string
		fast, slow func([]byte, string) []byte
		path       string
	}{
		{"roles", NormalizeContentRoles, normalizeContentRolesSlow, "contents"},
		{"parameters", RenameFunctionDeclarationParameters, renameFunctionDeclarationParametersSlow, "tools"},
		{"signatures", SkipThoughtSignatureValidation, skipThoughtSignatureValidationSlow, "contents"},
	}
	for i := 0; i < 3000; i++ {
		docs := []struct {
			raw    []byte
			prefix string
		}{
			{translatortest.GeminiRequest(r), ""},
			{translatortest.GeminiCLIRequest(r), "request."},
		}
		for _, doc := range docs {
			for _, e := range edits {
				path := doc.prefix + e.path
				got := e.fast(bytes.Clone(doc.raw), path)
				want := e.slow(bytes.Clone(doc.raw), path)
				if !bytes.Equal(got, want) {
					t.Fatalf("%s at %s differs on request %d\nrequest: %s\n    got: %s\n   want: %s", e.name, path, i, doc.raw, got, want)
				}
			}
		}
	}
}

func TestApplyRawPatchesRejectsMisplacedPatches(t *testing.T) {
	doc := []byte(`{"a":[{"x":1},{"y":2}]}`)
	good := RawPatch{start: 6, end: 13, orig: `{"x":1}`, repl: `{"x":10}`}
	if out, ok := ApplyRawPatches(doc, []RawPatch{good}); !ok || string(out) != `{"a":[{"x":10},{"y":2}]}` {
		t.Fatalf("a placed patch should apply: %s %v", out, ok)
	}
	for name, p := range map[string]RawPatch{
		"unplaced":       {start: 0, end: 7, orig: `{"x":1}`, repl: `{}`},
		"stale bytes":    {start: 6, end: 13, orig: `{"x":2}`, repl: `{}`},
		"past the end":   {start: 20, end: 27, orig: `{"x":1}`, repl: `{}`},
		"length differs": {start: 6, end: 12, orig: `{"x":1}`, repl: `{}`},
	} {
		if out, ok := ApplyRawPatches(doc, []RawPatch{p}); ok || !bytes.Equal(out, doc) {
			t.Fatalf("%s: a misplaced patch must leave the document alone, got %s %v", name, out, ok)
		}
	}
	overlapping := []RawPatch{good, {start: 8, end: 13, orig: `x":1}`, repl: `{}`}}
	if out, ok := ApplyRawPatches(doc, overlapping); ok || !bytes.Equal(out, doc) {
		t.Fatalf("overlapping patches must leave the document alone, got %s %v", out, ok)
	}
}
