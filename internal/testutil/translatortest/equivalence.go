// Package translatortest checks rewritten request translators against the
// translators they replaced. On 2026-11-16 the translators that appended every
// message to the whole request with sjson took two thirds of hk-155's CPU on
// large Codex sessions; the rewrites build arrays once instead and must keep
// every output byte, which these helpers verify on random requests of each
// input protocol.
package translatortest

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"regexp"
	"runtime"
	"testing"
)

// Translator is the signature every request translator has.
type Translator func(modelName string, rawJSON []byte, stream bool) []byte

// Equivalence describes one byte-for-byte comparison run.
type Equivalence struct {
	// Seed makes the run reproducible.
	Seed int64
	// Requests is how many random requests to compare; at least 3000.
	Requests int
	// Generate returns one random request.
	Generate func(r *rand.Rand) []byte
	// Models are passed to the translators in turn; nil means one empty name.
	Models []string
	// Mask, when set, rewrites both outputs before they are compared, for
	// translators that put random ids into the request.
	Mask func([]byte) []byte
}

// MatchReference fails on the first random request whose translations by got
// and want differ. Outputs that are both invalid JSON may differ: a request
// whose tool call arguments are not valid JSON is copied into an invalid
// request either way, and the old translators then misplaced every later
// element because they re-parsed the broken document to append each one. At
// most a tenth of the requests may be such, so the generator keeps exercising
// valid input.
func MatchReference(t testing.TB, e Equivalence, got, want Translator) {
	t.Helper()
	if e.Requests < 3000 {
		t.Fatalf("compare at least 3000 requests, not %d", e.Requests)
	}
	r := rand.New(rand.NewSource(e.Seed))
	models := e.Models
	if len(models) == 0 {
		models = []string{""}
	}
	broken := 0
	for i := 0; i < e.Requests; i++ {
		req := e.Generate(r)
		model := models[i%len(models)]
		stream := i%2 == 0
		g := got(model, bytes.Clone(req), stream)
		w := want(model, bytes.Clone(req), stream)
		if e.Mask != nil {
			g, w = e.Mask(g), e.Mask(w)
		}
		if bytes.Equal(g, w) {
			continue
		}
		if !json.Valid(g) && !json.Valid(w) {
			broken++
			continue
		}
		t.Fatalf("request %d (model %q, stream %v) differs from the reference\nrequest: %s\n    got: %s\n   want: %s", i, model, stream, req, g, w)
	}
	if broken > e.Requests/10 {
		t.Fatalf("%d of %d requests produced invalid JSON; the generator no longer exercises valid input", broken, e.Requests)
	}
	if broken > 0 {
		t.Logf("%d of %d requests produced invalid JSON in both translators", broken, e.Requests)
	}
}

// MaskRandomIDs replaces ids made of prefix and 24 random letters or digits,
// as the translators generate for tool calls that arrive without one.
func MaskRandomIDs(prefix string) func([]byte) []byte {
	re := regexp.MustCompile(regexp.QuoteMeta(prefix) + `[A-Za-z0-9]{24}`)
	repl := []byte(prefix + "RANDOM")
	return func(b []byte) []byte { return re.ReplaceAll(b, repl) }
}

// AllocatedBytes reports how many bytes f allocates.
func AllocatedBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Session builds a request with the given number of tool-call turns.
type Session func(turns int) []byte

// MatchReferenceOnSessions compares the translators on sessions of 0, 1, 7 and
// 40 turns.
func MatchReferenceOnSessions(t testing.TB, session Session, model string, mask func([]byte) []byte, got, want Translator) {
	t.Helper()
	for _, turns := range []int{0, 1, 7, 40} {
		req := session(turns)
		g := got(model, bytes.Clone(req), true)
		w := want(model, bytes.Clone(req), true)
		if mask != nil {
			g, w = mask(g), mask(w)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("%d turns differs from the reference\n got: %.2000s\nwant: %.2000s", turns, g, w)
		}
	}
}

// AssertGrowsLinearly fails when translating a session of 200 turns allocates
// more than eight times as much as one of 50. Appending every element to the
// whole request made the cost grow with the square of the session length,
// sixteen times for four times the history.
func AssertGrowsLinearly(t testing.TB, session Session, model string, translate Translator) {
	t.Helper()
	small, large := session(50), session(200)
	ratio := float64(AllocatedBytes(func() { translate(model, large, true) })) / float64(AllocatedBytes(func() { translate(model, small, true) }))
	if ratio > 8 {
		t.Fatalf("4x the history allocated %.1fx as much; the translator is quadratic again", ratio)
	}
}

// Benchmark translates a session of 300 turns b.N times, for the paired
// benchmarks that compare a rewritten translator with its reference in one
// test binary.
func Benchmark(b *testing.B, session Session, model string, translate Translator) {
	req := session(300)
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		translate(model, req, true)
	}
}
