package contentmoderation

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// idOf is the id masker gave the value secret, found by the placeholder a
// masker with the same key gives it.
func idOf(t *testing.T, masker *privacymask.Masker, seed byte, secret string) string {
	t.Helper()
	placeholder := placeholderOf(t, testMasker(seed), secret)
	for _, v := range masker.Values() {
		if v.Placeholder == placeholder {
			return v.ID
		}
	}
	t.Fatalf("no value has the placeholder of %.12s…", secret)
	return ""
}

// A request's record says where each value was first found: in the request
// itself or in the arguments of a tool call. The fields are worked out once
// the request is read, and the units they were worked out from let go.
func TestMaskNotesNameWhereEachValueWasFirstFound(t *testing.T) {
	arguments := `{"cmd":["sh","-c","export T=` + testAnthropicKey + `"]}`
	body := []byte(`{"model":"gpt","messages":[` +
		`{"role":"user","content":"my key is ` + testOpenAIKey + `"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":` + quoteJSON(arguments) + `}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"again ` + testOpenAIKey + `, and aws ` + testAWSKey + `"}]}`)
	masker := testMasker(6)
	notes := newMaskNotes()
	if masked, failure := maskRequest(sdktranslator.FormatOpenAI, body, masker, false, notes); masked == nil || failure != "" {
		t.Fatalf("masked = %s, failure = %q", masked, failure)
	}
	want := []struct{ secret, field string }{
		{testOpenAIKey, "messages[0].content"},
		{testAnthropicKey, "messages[1].tool_calls[0].function.arguments.cmd[2]"},
		{testAWSKey, "messages[2].content"},
	}
	if len(notes.fields) != len(want) || len(notes.snippets) != len(want) {
		t.Fatalf("fields = %q, %d snippets", notes.fields, len(notes.snippets))
	}
	for i, w := range want {
		if got := notes.fields[idOf(t, masker, 6, w.secret)]; got != w.field {
			t.Fatalf("value %d: field %q, want %q", i, got, w.field)
		}
		if got := notes.snippets[i].Field; got != w.field {
			t.Fatalf("snippet %d: field %q, want %q", i, got, w.field)
		}
	}
	if notes.places != nil || notes.placeOf != nil || notes.pending != nil {
		t.Fatal("the notes still hold the units, and through them the request")
	}
}

// A request masking refuses has a record too, which says where its values
// were, the one that stopped it included; the client is told where that one is.
func TestMaskNotesLocateTheValuesOfARefusedRequest(t *testing.T) {
	// A key as an object key of a tool's input is matched but not renamed.
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"aws ` + testAWSKey + `"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"` + testOpenAIKey + `":1}}]},{"role":"user","content":"go"}]}`)
	masker := testMasker(1)
	notes := newMaskNotes()
	masked, failure := maskRequest(sdktranslator.FormatClaude, body, masker, false, notes)
	if masked != nil || failure != "messages[1].content[0].input.<API_KEY> (an object key): it is not a plain string value" {
		t.Fatalf("masked = %s, failure = %q", masked, failure)
	}
	if got := notes.fields[idOf(t, masker, 1, testAWSKey)]; got != "messages[0].content[0].text" {
		t.Fatalf("field of the AWS key = %q", got)
	}
	if got := notes.fields[idOf(t, masker, 1, testOpenAIKey)]; got != "messages[1].content[0].input.<API_KEY> (an object key)" {
		t.Fatalf("field of the key that stopped it = %q", got)
	}
}

// The places of a request's values are found in one walk of each document
// they sit in, however many values there are, and masking a copy quietly
// walks nothing: a walk per value is what made masking a request of
// screenshots cost more than everything else about it.
func TestMaskingWalksEachDocumentOnce(t *testing.T) {
	walks := 0
	jsonPathWalks = func() { walks++ }
	defer func() { jsonPathWalks = nil }()
	for _, c := range []struct {
		name     string
		distinct bool
		want     int
	}{
		// the request, and the arguments of every call: each holds a new key
		{"new values every turn", true, 1 + 6},
		// the request, and the arguments of the first call: the values are
		// first found there
		{"the same values every turn", false, 1 + 1},
	} {
		body, _ := codexScreenshotRequest(6, 1, 1<<10, c.distinct)
		walks = 0
		notes := newMaskNotes()
		if masked, failure := maskRequest(sdktranslator.FormatOpenAI, body, testMasker(8), false, notes); masked == nil || failure != "" {
			t.Fatalf("%s: masked = %v, failure = %q", c.name, masked != nil, failure)
		}
		if walks != c.want {
			t.Fatalf("%s: %d walks for the fields of %d values, want %d", c.name, walks, len(notes.fields), c.want)
		}
		walks = 0
		if masked, failure := maskRequest(sdktranslator.FormatOpenAI, body, testMasker(8), true, nil); masked == nil || failure != "" {
			t.Fatalf("%s, quiet: masked = %v, failure = %q", c.name, masked != nil, failure)
		}
		if walks != 0 {
			t.Fatalf("%s: masking a copy quietly walked the request %d times", c.name, walks)
		}
	}
}
