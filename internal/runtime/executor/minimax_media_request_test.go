package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestShapeMiniMaxImageRequestConvertsSize(t *testing.T) {
	shaped, dropped := shapeMiniMaxImageRequest([]byte(`{"model":"image-01","prompt":"a cat","size":"1024x768"}`))

	parsed := gjson.ParseBytes(shaped)
	if parsed.Get("width").Int() != 1024 || parsed.Get("height").Int() != 768 {
		t.Errorf("dimensions = %dx%d, want 1024x768", parsed.Get("width").Int(), parsed.Get("height").Int())
	}
	if parsed.Get("size").Exists() {
		t.Error("size is not an accepted field and must not be forwarded")
	}
	if len(dropped) != 1 || dropped[0] != "size" {
		t.Errorf("dropped = %v, want [size] so the operator learns it was translated", dropped)
	}
}

// TestShapeMiniMaxImageRequestLetsAspectRatioWin mirrors the upstream precedence
// rule: sending derived pixels alongside a ratio would contradict a caller who
// already expressed the shape they want.
func TestShapeMiniMaxImageRequestLetsAspectRatioWin(t *testing.T) {
	shaped, _ := shapeMiniMaxImageRequest([]byte(`{"prompt":"a cat","size":"1024x768","aspect_ratio":"16:9"}`))

	parsed := gjson.ParseBytes(shaped)
	if parsed.Get("width").Exists() || parsed.Get("height").Exists() {
		t.Error("an explicit aspect_ratio must not be contradicted by derived dimensions")
	}
	if parsed.Get("aspect_ratio").String() != "16:9" {
		t.Error("aspect_ratio must be preserved")
	}
}

// TestShapeMiniMaxImageRequestKeepsExplicitDimensions covers the caller who already
// speaks the upstream shape.
func TestShapeMiniMaxImageRequestKeepsExplicitDimensions(t *testing.T) {
	shaped, _ := shapeMiniMaxImageRequest([]byte(`{"prompt":"a cat","size":"1024x768","width":512,"height":512}`))

	parsed := gjson.ParseBytes(shaped)
	if parsed.Get("width").Int() != 512 || parsed.Get("height").Int() != 512 {
		t.Errorf("dimensions = %dx%d, want the explicit 512x512", parsed.Get("width").Int(), parsed.Get("height").Int())
	}
}

// TestShapeMiniMaxImageRequestDropsUnparseableSizes covers "auto" and friends: they
// carry no pixel pair, so there is nothing to translate.
func TestShapeMiniMaxImageRequestDropsUnparseableSizes(t *testing.T) {
	for _, size := range []string{"auto", "1024", "widexhigh", "0x0"} {
		shaped, _ := shapeMiniMaxImageRequest([]byte(`{"prompt":"a cat","size":"` + size + `"}`))
		parsed := gjson.ParseBytes(shaped)
		if parsed.Get("width").Exists() || parsed.Get("height").Exists() {
			t.Errorf("size %q: invented dimensions %s", size, shaped)
		}
		if parsed.Get("size").Exists() {
			t.Errorf("size %q: must not be forwarded", size)
		}
	}
}

func TestShapeMiniMaxImageRequestMapsResponseFormat(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"x"}`: "base64",
		`{"prompt":"x","response_format":"b64_json"}`: "base64",
		`{"prompt":"x","response_format":"base64"}`:   "base64",
		`{"prompt":"x","response_format":"url"}`:      "url",
		`{"prompt":"x","response_format":"webp"}`:     "base64",
	}
	for payload, want := range cases {
		shaped, _ := shapeMiniMaxImageRequest([]byte(payload))
		if got := gjson.GetBytes(shaped, "response_format").String(); got != want {
			t.Errorf("%s: response_format = %q, want %q", payload, got, want)
		}
	}
}

func TestShapeMiniMaxImageRequestKeepsAcceptedFields(t *testing.T) {
	payload := []byte(`{"model":"image-01","prompt":"a cat","aspect_ratio":"1:1","seed":7,` +
		`"n":3,"prompt_optimizer":true,"user":"someone","quality":"high"}`)

	shaped, dropped := shapeMiniMaxImageRequest(payload)

	parsed := gjson.ParseBytes(shaped)
	for _, field := range []string{"model", "prompt", "aspect_ratio", "seed", "n", "prompt_optimizer"} {
		if !parsed.Get(field).Exists() {
			t.Errorf("accepted field %q was dropped: %s", field, shaped)
		}
	}
	// The endpoint rejects the whole request when an unsupported argument is
	// present, so unknown fields have to go — and the caller has to be told.
	for _, field := range []string{"user", "quality"} {
		if parsed.Get(field).Exists() {
			t.Errorf("unsupported field %q was forwarded: %s", field, shaped)
		}
	}
	if len(dropped) != 2 {
		t.Errorf("dropped = %v, want both unsupported fields reported", dropped)
	}
}

func TestShapeMiniMaxImageRequestLeavesNonObjectsAlone(t *testing.T) {
	payload := []byte(`[1,2,3]`)
	shaped, dropped := shapeMiniMaxImageRequest(payload)
	if string(shaped) != string(payload) || dropped != nil {
		t.Errorf("a non-object body must be left untouched, got %s / %v", shaped, dropped)
	}
}
