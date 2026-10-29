package contentmoderation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// moderationsServer records each input it receives and flags inputs that
// contain "FLAGGED".
func moderationsServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var inputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input string `json:"input"`
		}
		_ = decodeJSON(r, &body)
		mu.Lock()
		inputs = append(inputs, body.Input)
		mu.Unlock()
		score := "0.01"
		if strings.Contains(body.Input, "FLAGGED") {
			score = "0.99"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"violence":` + score + `}}]}`))
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), inputs...)
	}
}

func openAIProfile(t *testing.T, baseURL string) Profile {
	t.Helper()
	profile := testProfile(t, "tenant-a", "openai", ModePreBlock, KeywordModeAPIOnly)
	profile.BaseURL = baseURL
	return profile
}

// The whole request used to go out in one call, however long. Past the cap the
// newest text is sent, ending with the final question.
func TestOpenAIBackendSendsTheNewestTextWithinTheCap(t *testing.T) {
	server, inputs := moderationsServer(t)
	profile := openAIProfile(t, server.URL)
	input := ExtractModerationInput(sdktranslator.FormatClaude, claudeAgentRequest(t, "FLAGGED final question"))
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, input)
	if !decision.WouldBlock || decision.Action != ActionAPIBlock {
		t.Fatalf("decision = %#v, want the final question scanned and blocked", decision)
	}
	sent := inputs()
	if len(sent) != 1 {
		t.Fatalf("moderation calls = %d, want 1", len(sent))
	}
	if n := utf8.RuneCountInString(sent[0]); n > openAIModerationMaxInputChars {
		t.Fatalf("sent %d characters, cap %d", n, openAIModerationMaxInputChars)
	}
	if !strings.HasSuffix(sent[0], "FLAGGED final question") {
		t.Fatalf("input does not end with the newest user message: ...%q", sent[0][len(sent[0])-60:])
	}
}

func TestBackendSeesToolOutputOnlyWhenOptedIn(t *testing.T) {
	server, inputs := moderationsServer(t)
	profile := openAIProfile(t, server.URL)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"FLAGGED tool output"},{"type":"text","text":"what does it say?"}]}]}`)
	input := ExtractModerationInput(sdktranslator.FormatClaude, body)

	if decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, input); decision.WouldBlock {
		t.Fatalf("tool output reached the backend by default: %#v", decision)
	}
	if sent := inputs(); len(sent) != 1 || sent[0] != "what does it say?" {
		t.Fatalf("sent = %q, want the user text only", sent)
	}

	profile.APIIncludesToolOutput = true
	if decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, input); !decision.WouldBlock {
		t.Fatalf("opted-in tool output was not moderated: %#v", decision)
	}
	if sent := inputs(); len(sent) != 2 || sent[1] != "FLAGGED tool output what does it say?" {
		t.Fatalf("sent = %q", sent)
	}
}

// Assistant history is checked against keywords only; it never reaches the
// backend, even with tool output opted in.
func TestBackendNeverSeesAssistantHistory(t *testing.T) {
	server, inputs := moderationsServer(t)
	profile := openAIProfile(t, server.URL)
	profile.APIIncludesToolOutput = true
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"FLAGGED reply"},{"role":"user","content":"thanks"}]}`)
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, ExtractModerationInput(sdktranslator.FormatOpenAI, body))
	if decision.WouldBlock {
		t.Fatalf("assistant history reached the backend: %#v", decision)
	}
	if sent := inputs(); len(sent) != 1 || sent[0] != "hi thanks" {
		t.Fatalf("sent = %q", sent)
	}

	// With nothing but history there is nothing to send at all.
	historyOnly := []byte(`{"messages":[{"role":"assistant","content":"FLAGGED reply"}]}`)
	NewEvaluator(server.Client()).Evaluate(context.Background(), profile, ExtractModerationInput(sdktranslator.FormatOpenAI, historyOnly))
	if sent := inputs(); len(sent) != 1 {
		t.Fatalf("a history-only request called the backend: %q", sent)
	}
}

// Keyword checks read all three classes even when the backend reads one.
func TestKeywordAndAPIModeChecksEveryClassForKeywords(t *testing.T) {
	server, inputs := moderationsServer(t)
	profile := openAIProfile(t, server.URL)
	profile.KeywordMode = KeywordModeKeywordAndAPI
	profile.BlockedKeywords = []string{"forbidden"}
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a forbidden reply"},{"role":"user","content":"go on"}]}`)
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, ExtractModerationInput(sdktranslator.FormatOpenAI, body))
	if !decision.WouldBlock || decision.Action != ActionKeywordBlock || decision.MatchedSource != ClassAssistantHistory.String() {
		t.Fatalf("decision = %#v", decision)
	}
	if sent := inputs(); len(sent) != 0 {
		t.Fatalf("a keyword block still called the backend: %q", sent)
	}
}

// A zero-width space can split a word into tokens a classifier does not
// recognize, so the backend gets the text without them.
func TestBackendInputDropsIgnorableCharacters(t *testing.T) {
	server, inputs := moderationsServer(t)
	profile := openAIProfile(t, server.URL)
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, TextInput("FLAG\u200bGED text\ufeff"))
	if !decision.WouldBlock {
		t.Fatalf("decision = %#v", decision)
	}
	if sent := inputs(); len(sent) != 1 || sent[0] != "FLAGGED text" {
		t.Fatalf("sent = %q", sent)
	}
}
