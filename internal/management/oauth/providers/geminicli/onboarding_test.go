package geminicli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	geminiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
)

func TestPerformSetupUsesBackendProjectForFreeUser(t *testing.T) {
	transport := &sequenceTransport{t: t}
	transport.enqueue(func(req *http.Request) *http.Response {
		if req.Method != http.MethodPost || req.URL.Path != "/v1internal:loadCodeAssist" {
			t.Fatalf("request = %s %s, want loadCodeAssist", req.Method, req.URL.Path)
		}
		assertGeminiHeaders(t, req)
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"cloudaicompanionProject":"gen-lang-client-front"`) {
			t.Fatalf("loadCodeAssist body = %s, want requested project", string(body))
		}
		return jsonResponse(http.StatusOK, `{"allowedTiers":[{"id":"FREE","isDefault":true}]}`)
	})
	transport.enqueue(func(req *http.Request) *http.Response {
		if req.Method != http.MethodPost || req.URL.Path != "/v1internal:onboardUser" {
			t.Fatalf("request = %s %s, want onboardUser", req.Method, req.URL.Path)
		}
		assertGeminiHeaders(t, req)
		return jsonResponse(http.StatusOK, `{"done":true,"response":{"cloudaicompanionProject":"backend-project"}}`)
	})

	storage := &geminiauth.GeminiTokenStorage{}
	err := PerformSetup(context.Background(), &http.Client{Transport: transport}, storage, "gen-lang-client-front")
	if err != nil {
		t.Fatalf("PerformSetup() error = %v", err)
	}
	if storage.ProjectID != "backend-project" {
		t.Fatalf("ProjectID = %q, want backend-project", storage.ProjectID)
	}
	transport.assertDone()
}

func TestCheckCloudAPIIsEnabledEnablesDisabledService(t *testing.T) {
	transport := &sequenceTransport{t: t}
	transport.enqueue(func(req *http.Request) *http.Response {
		if req.Method != http.MethodGet || !strings.Contains(req.URL.Path, "/services/cloudaicompanion.googleapis.com") {
			t.Fatalf("request = %s %s, want service state", req.Method, req.URL.Path)
		}
		assertGeminiUserAgent(t, req)
		return jsonResponse(http.StatusOK, `{"state":"DISABLED"}`)
	})
	transport.enqueue(func(req *http.Request) *http.Response {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/services/cloudaicompanion.googleapis.com:enable") {
			t.Fatalf("request = %s %s, want service enable", req.Method, req.URL.Path)
		}
		assertGeminiUserAgent(t, req)
		return jsonResponse(http.StatusOK, `{}`)
	})

	enabled, err := CheckCloudAPIIsEnabled(context.Background(), &http.Client{Transport: transport}, "project-1")
	if err != nil {
		t.Fatalf("CheckCloudAPIIsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("CheckCloudAPIIsEnabled() = false, want true")
	}
	transport.assertDone()
}

type sequenceTransport struct {
	t     *testing.T
	steps []func(*http.Request) *http.Response
}

func (s *sequenceTransport) enqueue(fn func(*http.Request) *http.Response) {
	s.steps = append(s.steps, fn) //nolint:bodyclose // RoundTrip hands the response to http.Client; tested code closes the body.
}

func (s *sequenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.t.Helper()
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
	}
	fn := s.steps[0]
	s.steps = s.steps[1:]
	return fn(req), nil
}

func (s *sequenceTransport) assertDone() {
	s.t.Helper()
	if len(s.steps) != 0 {
		s.t.Fatalf("%d expected requests were not sent", len(s.steps))
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertGeminiHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	assertGeminiUserAgent(t, req)
	if got := req.Header.Get("X-Goog-Api-Client"); got != apiClient {
		t.Fatalf("X-Goog-Api-Client = %q, want %q", got, apiClient)
	}
	if got := req.Header.Get("Client-Metadata"); got != clientMetadata {
		t.Fatalf("Client-Metadata = %q, want %q", got, clientMetadata)
	}
}

func assertGeminiUserAgent(t *testing.T, req *http.Request) {
	t.Helper()
	if got := req.Header.Get("User-Agent"); got != userAgent {
		t.Fatalf("User-Agent = %q, want %q", got, userAgent)
	}
}
