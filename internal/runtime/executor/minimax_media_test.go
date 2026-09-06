package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func minimaxAPIKeyAuth(baseURL string) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{
		Provider:   "minimax",
		Attributes: map[string]string{"api_key": "test-key"},
	}
	if baseURL != "" {
		auth.Attributes["base_url"] = baseURL
	}
	return auth
}

// TestMiniMaxImageEndpointAddsTheVersionSegmentOnce covers both credential shapes
// that reach this executor: one configured for chat, which already carries the
// version segment, and one pointed at a bare host.
func TestMiniMaxImageEndpointAddsTheVersionSegmentOnce(t *testing.T) {
	cases := map[string]string{
		"":                              minimaxDefaultBaseURL + "/image_generation",
		"https://relay.example.com/v1":  "https://relay.example.com/v1/image_generation",
		"https://relay.example.com/v1/": "https://relay.example.com/v1/image_generation",
		"https://relay.example.com":     "https://relay.example.com/v1/image_generation",
	}
	for baseURL, want := range cases {
		if got := minimaxImageEndpoint(minimaxAPIKeyAuth(baseURL)); got != want {
			t.Errorf("base_url %q: endpoint = %q, want %q", baseURL, got, want)
		}
	}
}

// TestMiniMaxOnlyClaimsGeneration pins the boundary: the reference-image form is a
// different upstream request shape, so an edit request must not be answered here
// with a text-to-image result.
func TestMiniMaxOnlyClaimsGeneration(t *testing.T) {
	if !minimaxIsMediaAlt(minimaxImageGenerationAlt) {
		t.Error("the generation alt must route to the image endpoint")
	}
	for _, alt := range []string{"", "images/edits", "responses"} {
		if minimaxIsMediaAlt(alt) {
			t.Errorf("alt %q must not route to the image endpoint", alt)
		}
	}
}

func TestMiniMaxImageGenerationTranslatesRequestAndResponse(t *testing.T) {
	var gotPath, gotAuth, gotContentType, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"trace-1","data":{"image_base64":["AAAA","BBBB"]},` +
			`"metadata":{"success_count":2,"failed_count":0},"base_resp":{"status_code":0,"status_msg":"success"}}`))
	}))
	t.Cleanup(upstream.Close)

	resp, err := NewMiniMaxExecutor("minimax", &config.Config{}).Execute(
		context.Background(),
		minimaxAPIKeyAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{
			Model:   "image-01",
			Payload: []byte(`{"model":"image-01","prompt":"a cat","size":"1024x768","quality":"high","n":2}`),
		},
		cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotPath != "/v1/image_generation" {
		t.Errorf("upstream path = %q, want /v1/image_generation", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want a bearer credential", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	// size is not an accepted field, so the caller's dimensions have to survive as
	// width and height rather than being dropped with the unsupported arguments.
	sent := gjson.Parse(gotBody)
	if sent.Get("width").Int() != 1024 || sent.Get("height").Int() != 768 {
		t.Errorf("dimensions = %dx%d, want 1024x768", sent.Get("width").Int(), sent.Get("height").Int())
	}
	if sent.Get("size").Exists() || sent.Get("quality").Exists() {
		t.Errorf("unsupported arguments were forwarded: %s", gotBody)
	}
	if got := sent.Get("response_format").String(); got != "base64" {
		t.Errorf("response_format = %q, want base64", got)
	}
	if sent.Get("prompt").String() != "a cat" || sent.Get("n").Int() != 2 {
		t.Errorf("accepted arguments were not preserved: %s", gotBody)
	}

	// The images arrive as a bare array under data, which is not the shape the
	// public endpoint returns.
	translated := gjson.ParseBytes(resp.Payload)
	if got := translated.Get("data.0.b64_json").String(); got != "AAAA" {
		t.Errorf("data.0.b64_json = %q, want AAAA", got)
	}
	if got := translated.Get("data.1.b64_json").String(); got != "BBBB" {
		t.Errorf("data.1.b64_json = %q, want BBBB", got)
	}
	if got := len(translated.Get("data").Array()); got != 2 {
		t.Errorf("translated data length = %d, want 2", got)
	}

	// The body was rewritten, so the upstream length must not be forwarded.
	if resp.Headers.Get("Content-Length") != "" {
		t.Error("the upstream Content-Length no longer describes the translated body")
	}
}

// TestMiniMaxImageGenerationPreservesAnExplicitURLFormat covers the caller who
// wants links rather than bytes, despite the 24-hour expiry.
func TestMiniMaxImageGenerationPreservesAnExplicitURLFormat(t *testing.T) {
	var gotFormat string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotFormat = gjson.GetBytes(body, "response_format").String()
		_, _ = w.Write([]byte(`{"data":{"image_urls":["https://example.com/a.png"]},` +
			`"metadata":{"success_count":1,"failed_count":0},"base_resp":{"status_code":0}}`))
	}))
	t.Cleanup(upstream.Close)

	resp, err := NewMiniMaxExecutor("minimax", &config.Config{}).Execute(
		context.Background(),
		minimaxAPIKeyAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{
			Model:   "image-01",
			Payload: []byte(`{"model":"image-01","prompt":"a cat","response_format":"url"}`),
		},
		cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotFormat != "url" {
		t.Errorf("response_format = %q, want the caller's choice of url", gotFormat)
	}
	if got := gjson.ParseBytes(resp.Payload).Get("data.0.url").String(); got != "https://example.com/a.png" {
		t.Errorf("data.0.url = %q, want the upstream link", got)
	}
}

// TestMiniMaxImageGenerationMapsInBandFailures is the load-bearing rule: failures
// arrive with HTTP 200 and a non-zero base_resp status, so a pass-through would
// report an authentication or balance failure as a success with no images.
func TestMiniMaxImageGenerationMapsInBandFailures(t *testing.T) {
	cases := []struct {
		upstreamCode int
		wantStatus   int
	}{
		{1002, http.StatusTooManyRequests},
		{1004, http.StatusUnauthorized},
		{2049, http.StatusUnauthorized},
		{1008, http.StatusPaymentRequired},
		{1026, http.StatusBadRequest},
		{2013, http.StatusBadRequest},
		{9999, http.StatusBadGateway},
	}
	for _, tc := range cases {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{},"base_resp":{"status_code":` +
				strconv.Itoa(tc.upstreamCode) + `,"status_msg":"upstream refused"}}`))
		}))

		_, err := NewMiniMaxExecutor("minimax", &config.Config{}).Execute(
			context.Background(),
			minimaxAPIKeyAuth(upstream.URL+"/v1"),
			cliproxyexecutor.Request{Model: "image-01", Payload: []byte(`{"prompt":"x"}`)},
			cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
		)
		upstream.Close()

		if err == nil {
			t.Fatalf("status %d: an in-band failure must surface as an error", tc.upstreamCode)
		}
		coder, ok := err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("status %d: error does not carry a status code: %T", tc.upstreamCode, err)
		}
		if coder.StatusCode() != tc.wantStatus {
			t.Errorf("status %d: mapped to %d, want %d", tc.upstreamCode, coder.StatusCode(), tc.wantStatus)
		}
		if !strings.Contains(err.Error(), "upstream refused") {
			t.Errorf("status %d: the upstream message was lost: %v", tc.upstreamCode, err)
		}
	}
}

// TestMiniMaxImageGenerationRejectsAnEmptyResult keeps a blocked prompt from
// rendering as a successful call that produced nothing.
func TestMiniMaxImageGenerationRejectsAnEmptyResult(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"image_base64":[]},` +
			`"metadata":{"success_count":0,"failed_count":2},"base_resp":{"status_code":0}}`))
	}))
	t.Cleanup(upstream.Close)

	_, err := NewMiniMaxExecutor("minimax", &config.Config{}).Execute(
		context.Background(),
		minimaxAPIKeyAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{Model: "image-01", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
	)
	if err == nil {
		t.Fatal("a response carrying no images must surface as an error")
	}
	coder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error does not carry a status code: %T", err)
	}
	// Rejected content is the caller's request to fix, not an upstream fault.
	if coder.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", coder.StatusCode())
	}
}

func TestMiniMaxImageGenerationRejectsMissingCredential(t *testing.T) {
	_, err := NewMiniMaxExecutor("minimax", &config.Config{}).Execute(
		context.Background(),
		&cliproxyauth.Auth{Provider: "minimax"},
		cliproxyexecutor.Request{Model: "image-01", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
	)
	if err == nil {
		t.Fatal("a credential without an api key must be rejected")
	}
}

func TestMiniMaxImageGenerationStreamEmitsASingleChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"image_base64":["AAAA"]},"base_resp":{"status_code":0}}`))
	}))
	t.Cleanup(upstream.Close)

	result, err := NewMiniMaxExecutor("minimax", &config.Config{}).ExecuteStream(
		context.Background(),
		minimaxAPIKeyAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{Model: "image-01", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: minimaxImageGenerationAlt},
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	// The endpoint has no streaming form; callers going through the shared images
	// handler should still get a well-formed terminal chunk instead of an error.
	count := 0
	for chunk := range result.Chunks {
		count++
		if !strings.Contains(string(chunk.Payload), "b64_json") {
			t.Errorf("chunk %d did not carry the translated image: %s", count, chunk.Payload)
		}
	}
	if count != 1 {
		t.Errorf("chunk count = %d, want 1", count)
	}
}
