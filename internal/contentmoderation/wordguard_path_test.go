package contentmoderation

import (
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestJSONPathAt(t *testing.T) {
	tests := []struct {
		name string
		data string
		// at is the text the offset points at: the first place it appears.
		at   string
		want string
	}{
		{"a value in an object", `{"a":{"b":[1,{"c":"x"}]}}`, `"x"`, "a.b[1].c"},
		{"a key", `{"a":{"b":[1,{"c":"x"}]}}`, `"c"`, "a.b[1].c"},
		{"an array element", `{"list":["p","q","r"]}`, `"r"`, "list[2]"},
		{"an array of arrays", `[[1,2],[3,"z"]]`, `"z"`, "[1][1]"},
		{"a number", `{"a":[10,20]}`, `20`, "a[1]"},
		{"an object that is a value", `{"a":{"b":1}}`, `{"b"`, "a"},
		{"keys that need quoting", `{"a.b":{"x y":"v"}}`, `"v"`, `["a.b"]["x y"]`},
		{"a key with a digit and a dash", `{"k-1":{"2x":"v"}}`, `"v"`, `k-1["2x"]`},
		{"a key written with escapes", `{"b\u0061d":"v"}`, `"v"`, "bad"},
		{"an escaped quote does not end the string", `{"q\"uote":"a \"x\" b","n":"here"}`, `"here"`, "n"},
		{"white space everywhere", "{ \"a\" :\n\t[ 1 ,\r\n { \"c\" : \"x\" } ] }", `"x"`, "a[1].c"},
		{"literals before the target", `{"t":true,"f":false,"n":null,"v":"x"}`, `"x"`, "v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset := strings.Index(tt.data, tt.at)
			if offset < 0 {
				t.Fatalf("test data: %q is not in %q", tt.at, tt.data)
			}
			if got := jsonPathAt([]byte(tt.data), offset); got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJSONPathAtGivesUpQuietly(t *testing.T) {
	deep := strings.Repeat("[", maxJSONPathDepth+10) + `"x"` + strings.Repeat("]", maxJSONPathDepth+10)
	tests := []struct {
		name   string
		data   string
		offset int
	}{
		{"the middle of a string", `{"a":"hello"}`, 8},
		{"past the end", `{"a":1}`, 99},
		{"a negative offset", `{"a":1}`, -1},
		{"a document that is cut short", `{"a":{"b":`, 9},
		{"not a document", `hello`, 2},
		{"an object missing its colon", `{"a" 1}`, 5},
		{"nested deeper than anything real", deep, strings.Index(deep, `"x"`)},
		{"the document itself", `{"a":1}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonPathAt([]byte(tt.data), tt.offset); got != "" {
				t.Fatalf("path = %q, want none", got)
			}
		})
	}
}

func TestJoinJSONPath(t *testing.T) {
	tests := []struct{ outer, inner, want string }{
		{"messages[0].function.arguments", "q", "messages[0].function.arguments.q"},
		{"messages[0].function.arguments", "[2].q", "messages[0].function.arguments[2].q"},
		{"messages[0].function.arguments", `["a b"]`, `messages[0].function.arguments["a b"]`},
		{"", "q", "q"},
		{"messages[0]", "", "messages[0]"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := joinJSONPath(tt.outer, tt.inner); got != tt.want {
			t.Errorf("joinJSONPath(%q, %q) = %q, want %q", tt.outer, tt.inner, got, tt.want)
		}
	}
}

// A request names the field of a unit that sits in a tool call's arguments by
// the path of the arguments string and then the path inside the JSON it holds.
func TestWordDocPathThroughNestedArguments(t *testing.T) {
	payload := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x","tool_calls":[{"function":{"name":"f","arguments":"{\"list\":[\"a\",{\"deep key\":\"here\"}]}"}}]}]}`
	units := extractWordUnits(sdktranslator.FormatOpenAI, []byte(payload))
	var found bool
	for _, unit := range units {
		if unit.text != "here" {
			continue
		}
		found = true
		if got, want := unitField(unit), `messages[1].tool_calls[0].function.arguments.list[1]["deep key"]`; got != want {
			t.Fatalf("field = %q, want %q", got, want)
		}
	}
	if !found {
		t.Fatalf("no unit for the nested text: %q", describeUnits(units))
	}
}
