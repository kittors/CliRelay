package executor

import (
	"net/http"
	"testing"
)

func TestGenerateStableSessionID(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}}`)

	// Deterministic: same first turn + generation -> same session id, so a
	// conversation reuses one upstream session and keeps its prefix-cache hits.
	id0a := generateStableSessionID(payload, 0)
	id0b := generateStableSessionID(payload, 0)
	if id0a != id0b {
		t.Fatalf("generation 0 not deterministic: %q vs %q", id0a, id0b)
	}

	// Bumping the generation rotates the id for the same conversation, which is
	// how a request recovers from the per-session 1,048,576-token wall.
	id1 := generateStableSessionID(payload, 1)
	id2 := generateStableSessionID(payload, 2)
	if id1 == id0a || id2 == id0a || id1 == id2 {
		t.Fatalf("bumped generations must differ: gen0=%q gen1=%q gen2=%q", id0a, id1, id2)
	}

	// A different first user turn yields a different session.
	other := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"different"}]}]}}`)
	if generateStableSessionID(other, 0) == id0a {
		t.Fatalf("different first turn should produce a different session id")
	}

	// Top-level contents (no request wrapper) is accepted and matches.
	flat := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`)
	if generateStableSessionID(flat, 0) != id0a {
		t.Fatalf("flat contents should match wrapped contents for the same text")
	}

	// All ids are the negative-integer form the upstream expects.
	for _, id := range []string{id0a, id1, id2} {
		if len(id) < 2 || id[0] != '-' {
			t.Fatalf("session id %q is not a negative integer string", id)
		}
	}
}

func TestAntigravityShouldBumpSession(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		body   string
		expect bool
	}{
		{"exceeds-by-number", http.StatusBadRequest, `{"error":{"message":"The input token count exceeds the maximum number of tokens allowed 1048576"}}`, true},
		{"exceeds-by-text", http.StatusBadRequest, `{"error":{"message":"input exceeds the maximum number of tokens"}}`, true},
		{"other-400", http.StatusBadRequest, `{"error":{"message":"Request contains an invalid argument."}}`, false},
		{"429-with-number", http.StatusTooManyRequests, `1048576`, false},
		{"200-with-number", http.StatusOK, `1048576`, false},
		{"empty-400", http.StatusBadRequest, ``, false},
	}
	for _, c := range cases {
		if got := antigravityShouldBumpSession(c.code, []byte(c.body)); got != c.expect {
			t.Errorf("%s: antigravityShouldBumpSession(%d) = %v, want %v", c.name, c.code, got, c.expect)
		}
	}
}
