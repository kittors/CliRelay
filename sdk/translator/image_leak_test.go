package translator

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestFindLeakedImage(t *testing.T) {
	data := strings.Repeat("A", minLeakedImageBytes)
	leaked := `{"messages":[{"content":[{"type":"tool_result","content":"[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,` + data + `\"}]"}]}]}`
	if path, _ := findLeakedImage(gjson.Parse(leaked), ""); path != "messages.0.content.0.content" {
		t.Fatalf("path = %q, want the tool_result content", path)
	}

	proper := `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]}]}`
	if path, _ := findLeakedImage(gjson.Parse(proper), ""); path != "" {
		t.Fatalf("a proper image field was reported at %q", path)
	}

	small := `{"text":"see data:image/png;base64,AAAA"}`
	if path, _ := findLeakedImage(gjson.Parse(small), ""); path != "" {
		t.Fatalf("a short mention was reported at %q", path)
	}
}
