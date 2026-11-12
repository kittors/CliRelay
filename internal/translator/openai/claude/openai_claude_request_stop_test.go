package claude

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToOpenAIStopSequences(t *testing.T) {
	cases := []struct {
		name, input string
		want        []string
	}{
		{"omitted", "", nil},
		{"empty", "[]", nil},
		{"null", "null", nil},
		{"non_array", `"stop"`, nil},
		{"one", `["</block>"]`, []string{"</block>"}},
		{"two", `["a","b"]`, []string{"a", "b"}},
		{"four", `["a","b","c","d"]`, []string{"a", "b", "c", "d"}},
		{"five_not_truncated", `["a","b","c","d","e"]`, []string{"a", "b", "c", "d", "e"}},
		{"empty_string", `[""]`, []string{""}},
		{"escaped_unicode", `["引号\"\\\n"]`, []string{"引号\"\\\n"}},
		{"duplicates", `["a","a"]`, []string{"a", "a"}},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				input := `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"ok"}]`
				if tc.input != "" {
					input += `,"stop_sequences":` + tc.input
				}
				input += "}"
				out := ConvertClaudeRequestToOpenAI("target", []byte(input), stream)
				stop := gjson.GetBytes(out, "stop")
				if tc.want == nil {
					if stop.Exists() {
						t.Fatalf("stop must be omitted: %s", out)
					}
				} else {
					if !stop.IsArray() {
						t.Fatalf("stop must remain an array: %s", out)
					}
					var got []string
					for _, v := range stop.Array() {
						got = append(got, v.String())
					}
					if !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("got %#v, want %#v", got, tc.want)
					}
				}
				if gjson.GetBytes(out, "stream").Bool() != stream {
					t.Fatal("stream changed")
				}
			})
		}
	}
}
