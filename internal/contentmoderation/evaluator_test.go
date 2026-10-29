package contentmoderation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvaluatorKeywordAndAPIModes(t *testing.T) {
	profile := testProfile(t, "tenant-a", "keywords", ModePreBlock, KeywordModeKeywordOnly)
	profile.BlockedKeywords = []string{"forbidden"}
	decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("contains FORBIDDEN text"))
	if !decision.WouldBlock || decision.Action != ActionKeywordBlock || decision.MatchedKeyword != "forbidden" {
		t.Fatalf("keyword decision = %#v", decision)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"hate":0.9,"violence":0.1}}]}`))
	}))
	defer server.Close()
	profile.BaseURL = server.URL
	profile.KeywordMode = KeywordModeAPIOnly
	decision = NewEvaluator(server.Client()).Evaluate(context.Background(), profile, TextInput("test input"))
	if !decision.WouldBlock || decision.Action != ActionAPIBlock || decision.HighestCategory != "hate" {
		t.Fatalf("api decision = %#v", decision)
	}
}

func TestEvaluatorFailsOpenOnModerationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	profile := testProfile(t, "tenant-a", "api", ModePreBlock, KeywordModeAPIOnly)
	profile.BaseURL = server.URL
	profile.TimeoutMS = int((100 * time.Millisecond) / time.Millisecond)
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, TextInput("test input"))
	if decision.WouldBlock || decision.Action != ActionAPIError || decision.ModerationError == "" {
		t.Fatalf("fail-open decision = %#v", decision)
	}
}
