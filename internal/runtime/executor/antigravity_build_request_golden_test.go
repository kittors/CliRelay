package executor

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var updateAntigravityBuildGolden = flag.Bool("update-antigravity-build-golden", false,
	"rewrite testdata/antigravity_build_request/*.golden.json from the current buildRequest")

const antigravityBuildGoldenDir = "testdata/antigravity_build_request"

// antigravityBuildGoldenModels reach each branch of buildRequest that shapes the
// body: the Claude family (Antigravity schema cleanup, VALIDATED tool mode), a
// Gemini model (Gemini schema cleanup, maxOutputTokens dropped) and
// gemini-3-pro-high, the one non-Claude model that takes the Antigravity cleanup.
var antigravityBuildGoldenModels = []string{"claude-sonnet-4-5", "gemini-2.5-pro", "gemini-3-pro-high"}

var antigravityGoldenRequestID = regexp.MustCompile(`"requestId":"agent-[0-9a-f-]{36}"`)

// TestAntigravityBuildRequestGolden pins the body buildRequest sends upstream,
// byte for byte. The schema cleanup used to rewrite the whole payload for every
// edit, so making it cheaper must not move a single byte; the golden files were
// written by the implementation that predates that change.
//
// The inputs are raw on purpose (tool schemas nobody has cleaned yet, function
// calls whose arguments use schema keywords as data, indented and oddly spaced
// JSON), because that is where a scoped cleanup could drift from the old one.
func TestAntigravityBuildRequestGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(antigravityBuildGoldenDir, "*.input.json"))
	if err != nil {
		t.Fatalf("glob inputs: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no inputs under %s", antigravityBuildGoldenDir)
	}
	for _, input := range inputs {
		payload, errRead := os.ReadFile(input)
		if errRead != nil {
			t.Fatalf("read %s: %v", input, errRead)
		}
		name := strings.TrimSuffix(filepath.Base(input), ".input.json")
		for _, model := range antigravityBuildGoldenModels {
			t.Run(name+"/"+model, func(t *testing.T) {
				body := buildAntigravityGoldenBody(t, model, payload)
				if again := buildAntigravityGoldenBody(t, model, payload); !bytes.Equal(body, again) {
					t.Fatalf("buildRequest is not deterministic for %s", name)
				}
				golden := filepath.Join(antigravityBuildGoldenDir, name+"."+model+".golden.json")
				if *updateAntigravityBuildGolden {
					if errWrite := os.WriteFile(golden, body, 0o644); errWrite != nil {
						t.Fatalf("write %s: %v", golden, errWrite)
					}
					return
				}
				want, errGolden := os.ReadFile(golden)
				if errGolden != nil {
					t.Fatalf("read %s: %v (run with -update-antigravity-build-golden to create it)", golden, errGolden)
				}
				if !bytes.Equal(body, want) {
					t.Fatalf("body differs from %s:\n%s", golden, describeFirstDifference(want, body))
				}
			})
		}
	}
}

// buildAntigravityGoldenBody runs buildRequest the way the executor does and
// returns the body with its per-call request id replaced, so that the rest of
// the body can be compared exactly.
func buildAntigravityGoldenBody(t *testing.T, model string, payload []byte) []byte {
	t.Helper()
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "golden-project"}}
	_, body, err := executor.buildRequest(context.Background(), auth, "token", model, bytes.Clone(payload), true, "", "https://example.com", 0)
	if err != nil {
		t.Fatalf("buildRequest(%s): %v", model, err)
	}
	return antigravityGoldenRequestID.ReplaceAll(body, []byte(`"requestId":"agent-00000000-0000-0000-0000-000000000000"`))
}

// describeFirstDifference shows where two bodies part ways, with some context,
// instead of dumping several kilobytes of JSON twice.
func describeFirstDifference(want, got []byte) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	clip := func(b []byte) string {
		end := i + 80
		if end > len(b) {
			end = len(b)
		}
		if start > len(b) {
			return ""
		}
		return string(b[start:end])
	}
	return fmt.Sprintf("first difference at byte %d (want %d bytes, got %d bytes)\nwant: ...%s...\n got: ...%s...",
		i, len(want), len(got), clip(want), clip(got))
}
