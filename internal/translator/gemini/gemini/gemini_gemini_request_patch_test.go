package gemini

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

// The patched tool renames must match the per-path renames they fall back to
// on every random request, including the ones whose tools the patch cannot
// place.
func TestRenameToolDeclarationsMatchesPerPathRenames(t *testing.T) {
	r := rand.New(rand.NewSource(20261116))
	for i := 0; i < 3000; i++ {
		req := translatortest.GeminiRequest(r)
		got := renameToolDeclarations(bytes.Clone(req))
		want := renameToolDeclarationsSlow(bytes.Clone(req))
		if !bytes.Equal(got, want) {
			t.Fatalf("request %d differs\nrequest: %s\n    got: %s\n   want: %s", i, req, got, want)
		}
	}
}
