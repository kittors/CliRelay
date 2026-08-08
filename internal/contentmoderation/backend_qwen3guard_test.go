package contentmoderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func decodeJSON(r *http.Request, target any) error {
	return json.NewDecoder(r.Body).Decode(target)
}

// recordedPaths collects request paths across the test server's goroutines.
type recordedPaths struct {
	mu     sync.Mutex
	values []string
}

func (p *recordedPaths) add(value string) {
	p.mu.Lock()
	p.values = append(p.values, value)
	p.mu.Unlock()
}

func (p *recordedPaths) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.values...)
}

func guardProfile(t *testing.T, baseURL string) Profile {
	t.Helper()
	profile, err := NewProfile("tenant-a", "profile-guard", CreateProfileInput{
		Name:        "guard",
		Mode:        ModePreBlock,
		Backend:     BackendQwen3Guard,
		BaseURL:     baseURL,
		Model:       "qwen3guard",
		KeywordMode: KeywordModeAPIOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	return profile
}

// guardServer replies with the given verdicts in order, recording the requests.
func guardServer(t *testing.T, verdicts ...string) (*httptest.Server, *recordedPaths, *atomic.Int32) {
	t.Helper()
	paths := &recordedPaths{}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		paths.add(r.URL.Path)
		verdict := verdicts[len(verdicts)-1]
		if index < len(verdicts) {
			verdict = verdicts[index]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(verdict) + `}}]}`))
	}))
	t.Cleanup(server.Close)
	return server, paths, &calls
}

func jsonString(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `"`, `\"`), "\n", `\n`) + `"`
}

func TestParseQwen3GuardVerdicts(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantSafety string
		wantKnown  []string
		wantErr    bool
	}{
		{"safe", "Safety: Safe\nCategories: None", safetySafe, nil, false},
		{"unsafe", "Safety: Unsafe\nCategories: Violent", safetyUnsafe, []string{ScannerViolent}, false},
		{"controversial", "Safety: Controversial\nCategories: PII", safetyControversial, []string{ScannerPII}, false},
		{"crlf and blank lines", "Safety: Unsafe\r\n\r\nCategories: Jailbreak\r\n", safetyUnsafe, []string{ScannerJailbreak}, false},
		{"lowercase labels", "safety: unsafe\ncategories: jailbreak", safetyUnsafe, []string{ScannerJailbreak}, false},
		{"refusal line ignored", "Safety: Unsafe\nCategories: Violent\nRefusal: Yes", safetyUnsafe, []string{ScannerViolent}, false},
		{"trailing prose ignored", "Safety: Safe\nCategories: None\nThis prompt is fine.", safetySafe, nil, false},
		{"n/a categories", "Safety: Safe\nCategories: N/A", safetySafe, nil, false},
		{"duplicate safety", "Safety: Safe\nSafety: Unsafe\nCategories: None", "", nil, true},
		{"duplicate categories", "Safety: Safe\nCategories: None\nCategories: PII", "", nil, true},
		{"missing categories", "Safety: Safe", "", nil, true},
		{"missing safety", "Categories: None", "", nil, true},
		{"unknown safety", "Safety: Maybe\nCategories: None", "", nil, true},
		{"empty", "", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict, err := parseQwen3Guard(tt.content)
			if tt.wantErr {
				if !errors.Is(err, errGuardInvalidResponse) {
					t.Fatalf("expected invalid verdict error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQwen3Guard: %v", err)
			}
			if verdict.Safety != tt.wantSafety {
				t.Fatalf("safety = %q, want %q", verdict.Safety, tt.wantSafety)
			}
			if len(verdict.Categories) != len(tt.wantKnown) {
				t.Fatalf("categories = %v, want %v", verdict.Categories, tt.wantKnown)
			}
			for i, category := range tt.wantKnown {
				if verdict.Categories[i] != category {
					t.Fatalf("categories = %v, want %v", verdict.Categories, tt.wantKnown)
				}
			}
		})
	}
}

// The nine labels come straight from the Qwen3Guard model card; if the catalog
// or the aliases drift, real verdicts would silently degrade into unknowns.
func TestParseQwen3GuardOfficialCategorySet(t *testing.T) {
	const official = "Violent, Non-violent Illegal Acts, Sexual Content or Sexual Acts, PII, " +
		"Suicide & Self-Harm, Unethical Acts, Politically Sensitive Topics, Copyright Violation, Jailbreak"
	verdict, err := parseQwen3Guard("Safety: Unsafe\nCategories: " + official)
	if err != nil {
		t.Fatalf("parseQwen3Guard: %v", err)
	}
	if len(verdict.UnknownCategories) != 0 {
		t.Fatalf("unknown categories = %v, want none", verdict.UnknownCategories)
	}
	if len(verdict.Categories) != len(AllScannerIDs) {
		t.Fatalf("categories = %v, want all %d", verdict.Categories, len(AllScannerIDs))
	}
	for i, category := range AllScannerIDs {
		if verdict.Categories[i] != category {
			t.Fatalf("categories = %v, want %v", verdict.Categories, AllScannerIDs)
		}
	}
}

// A truncated Categories line still parses (the prefix was emitted), so the
// leftover fragment must not be silently ignored: it becomes an unknown
// category, which keeps an Unsafe verdict blocking.
func TestParseQwen3GuardTruncatedCategoryLineBlocks(t *testing.T) {
	verdict, err := parseQwen3Guard("Safety: Unsafe\nCategories: Violent, Non-vio")
	if err != nil {
		t.Fatalf("parseQwen3Guard: %v", err)
	}
	if len(verdict.UnknownCategories) != 1 {
		t.Fatalf("unknown categories = %v, want one fragment", verdict.UnknownCategories)
	}
	if !strings.HasPrefix(verdict.UnknownCategories[0], "unknown:") {
		t.Fatalf("unknown category %q should be hashed", verdict.UnknownCategories[0])
	}
	profile := Profile{Scanners: []string{ScannerPII}}
	if _, block := decideGuardVerdict(profile, verdict); !block {
		t.Fatal("unsafe verdict with an unknown category must block")
	}
}

func TestDecideGuardVerdict(t *testing.T) {
	elevated := DefaultElevatedCategories()
	tests := []struct {
		name       string
		profile    Profile
		verdict    guardVerdict
		wantBlock  bool
		wantMatch  int
		wantReason string
	}{
		{
			name:      "safe allows",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetySafe},
			wantBlock: false,
		},
		{
			name:      "unsafe blocks",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyUnsafe, Categories: []string{ScannerViolent}},
			wantBlock: true, wantMatch: 1,
		},
		{
			name:      "unsafe with only disabled categories allows",
			profile:   Profile{Scanners: []string{ScannerPII}, ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyUnsafe, Categories: []string{ScannerViolent}},
			wantBlock: false,
		},
		{
			name:      "unsafe without any category blocks",
			profile:   Profile{Scanners: []string{ScannerPII}, ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyUnsafe},
			wantBlock: true,
		},
		{
			name:      "unsafe with unknown category blocks",
			profile:   Profile{Scanners: []string{ScannerPII}, ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyUnsafe, Categories: []string{ScannerViolent}, UnknownCategories: []string{"unknown:abcd"}},
			wantBlock: true,
		},
		{
			name:      "controversial jailbreak escalates",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerJailbreak}},
			wantBlock: true, wantMatch: 1,
		},
		{
			name:      "controversial pii escalates",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerPII}},
			wantBlock: true, wantMatch: 1,
		},
		{
			name:      "controversial self harm escalates",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerSuicideAndSelfHarm}},
			wantBlock: true, wantMatch: 1,
		},
		{
			name:      "controversial non-elevated allows",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerViolent}},
			wantBlock: false, wantMatch: 1,
		},
		{
			name:      "controversial elevated but scanner disabled allows",
			profile:   Profile{Scanners: []string{ScannerViolent}, ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerJailbreak}},
			wantBlock: false,
		},
		{
			name:      "controversial action block blocks anything",
			profile:   Profile{ControversialAction: ControversialActionBlock, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerViolent}},
			wantBlock: true, wantMatch: 1,
		},
		{
			name:      "controversial action allow never blocks",
			profile:   Profile{ControversialAction: ControversialActionAllow, ElevatedCategories: elevated},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerJailbreak}},
			wantBlock: false, wantMatch: 1,
		},
		{
			name:      "empty elevated list never escalates",
			profile:   Profile{ControversialAction: ControversialActionElevatedOnly, ElevatedCategories: []string{}},
			verdict:   guardVerdict{Safety: safetyControversial, Categories: []string{ScannerJailbreak}},
			wantBlock: false, wantMatch: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, block := decideGuardVerdict(tt.profile, tt.verdict)
			if block != tt.wantBlock {
				t.Fatalf("block = %v, want %v", block, tt.wantBlock)
			}
			if len(matched) != tt.wantMatch {
				t.Fatalf("matched = %v, want %d entries", matched, tt.wantMatch)
			}
		})
	}
}

func TestSplitForGuardChunksAndTruncates(t *testing.T) {
	// Multi-byte runes must not be split mid-character.
	input := strings.Repeat("中", 10)
	chunks, truncated := splitForGuard(input, 4, 2)
	if len(chunks) != 2 || truncated != 2 {
		t.Fatalf("chunks = %v truncated = %d", chunks, truncated)
	}
	if chunks[0] != strings.Repeat("中", 4) || chunks[1] != strings.Repeat("中", 4) {
		t.Fatalf("chunks = %v", chunks)
	}
	chunks, truncated = splitForGuard("short", 4000, 4)
	if len(chunks) != 1 || truncated != 0 {
		t.Fatalf("chunks = %v truncated = %d", chunks, truncated)
	}
	if chunks, truncated = splitForGuard("", 4000, 4); len(chunks) != 0 || truncated != 0 {
		t.Fatalf("empty input produced chunks = %v truncated = %d", chunks, truncated)
	}
}

func TestGuardBackendUsesChatCompletionsAndStopsAtFirstBlock(t *testing.T) {
	server, paths, calls := guardServer(t,
		"Safety: Safe\nCategories: None",
		"Safety: Unsafe\nCategories: Violent",
		"Safety: Safe\nCategories: None",
	)
	profile := guardProfile(t, server.URL)
	profile.InputLimit = MinInputLimit
	profile.MaxChunks = 4
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, strings.Repeat("a", MinInputLimit*3))
	if !decision.WouldBlock || decision.Action != ActionGuardBlock {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Safety != safetyUnsafe {
		t.Fatalf("safety = %q", decision.Safety)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("guard called %d times, want 2 (stop after the blocking chunk)", got)
	}
	for _, path := range paths.snapshot() {
		if path != "/v1/chat/completions" {
			t.Fatalf("guard requested %q, want /v1/chat/completions", path)
		}
	}
}

func TestGuardBackendNeverCallsModerationsEndpoint(t *testing.T) {
	server, paths, _ := guardServer(t, "Safety: Safe\nCategories: None")
	profile := guardProfile(t, server.URL)
	NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "hello")
	for _, path := range paths.snapshot() {
		if strings.Contains(path, "moderations") {
			t.Fatalf("qwen3guard backend must not call %q", path)
		}
	}
}

func TestGuardBackendRequestShape(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeJSON(r, &body)
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("unauthenticated profile sent Authorization %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: None"}}]}`))
	}))
	defer server.Close()
	profile := guardProfile(t, server.URL)
	NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "hello")
	if _, exists := body["seed"]; exists {
		t.Fatal("seed must not be sent: it is non-standard and some servers reject it")
	}
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != guardMaxTokens {
		t.Fatalf("max_tokens = %v, want %d (model card max_new_tokens)", body["max_tokens"], guardMaxTokens)
	}
	if got, ok := body["temperature"].(float64); !ok || got != 0 {
		t.Fatalf("temperature = %v, want 0", body["temperature"])
	}
}

func TestGuardBackendSendsBearerWhenConfigured(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: None"}}]}`))
	}))
	defer server.Close()
	profile := guardProfile(t, server.URL)
	profile.APIKeySecret = "guard-token"
	NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "hello")
	if authHeader != "Bearer guard-token" {
		t.Fatalf("Authorization = %q", authHeader)
	}
}

func TestGuardBackendFailsOpen(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		errorClass string
	}{
		{
			name: "upstream status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			errorClass: "upstream_status",
		},
		{
			name: "invalid verdict",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I cannot help with that."}}]}`))
			},
			errorClass: "invalid_response",
		},
		{
			name: "oversized body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("a", int(maxModerationResponseBytes)+16) + `"}}]}`))
			},
			errorClass: "invalid_response",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			profile := guardProfile(t, server.URL)
			decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "hello")
			if decision.WouldBlock || decision.Action != ActionAPIError {
				t.Fatalf("decision = %#v, want fail-open", decision)
			}
			if got := moderationErrorClass(decision.ModerationError); got != tt.errorClass {
				t.Fatalf("error class = %q, want %q (%s)", got, tt.errorClass, decision.ModerationError)
			}
		})
	}
}

// TimeoutMS budgets the whole evaluation, so a slow endpoint cannot multiply
// the promised latency by the chunk count.
func TestGuardBackendBudgetsAllChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: None"}}]}`))
	}))
	defer server.Close()
	profile := guardProfile(t, server.URL)
	profile.InputLimit = MinInputLimit
	profile.MaxChunks = 8
	profile.TimeoutMS = 200
	start := time.Now()
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, strings.Repeat("a", MinInputLimit*8))
	elapsed := time.Since(start)
	if decision.WouldBlock || decision.Action != ActionAPIError {
		t.Fatalf("decision = %#v, want fail-open on budget exhaustion", decision)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("evaluation took %s; the budget must cover all chunks", elapsed)
	}
}

func TestGuardBackendAggregatesAcrossChunks(t *testing.T) {
	server, _, _ := guardServer(t,
		"Safety: Safe\nCategories: None",
		"Safety: Controversial\nCategories: Violent",
		"Safety: Safe\nCategories: None",
	)
	profile := guardProfile(t, server.URL)
	profile.InputLimit = MinInputLimit
	profile.MaxChunks = 3
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, strings.Repeat("a", MinInputLimit*3))
	if decision.WouldBlock {
		t.Fatalf("decision = %#v, want allow (violent is not elevated)", decision)
	}
	if decision.Safety != safetyControversial {
		t.Fatalf("safety = %q, want the most severe chunk verdict", decision.Safety)
	}
	if decision.CategoryScores[ScannerViolent] != 0.5 {
		t.Fatalf("scores = %v, want controversial mapped to 0.5", decision.CategoryScores)
	}
	if decision.HighestCategory != ScannerViolent {
		t.Fatalf("highest category = %q", decision.HighestCategory)
	}
}

func TestExtractChatContentAcceptsStringAndBlocks(t *testing.T) {
	content, err := extractChatContent([]byte(`{"choices":[{"message":{"content":"Safety: Safe"}}]}`))
	if err != nil || content != "Safety: Safe" {
		t.Fatalf("content = %q err = %v", content, err)
	}
	content, err = extractChatContent([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"Safety: Safe"},{"type":"text","text":"Categories: None"}]}}]}`))
	if err != nil || content != "Safety: Safe\nCategories: None" {
		t.Fatalf("content = %q err = %v", content, err)
	}
	for _, body := range []string{`{}`, `{"choices":[]}`, `{"choices":[{"message":{"content":null}}]}`, `{"choices":[{"message":{"content":"  "}}]}`, `not json`} {
		if _, err = extractChatContent([]byte(body)); err == nil {
			t.Fatalf("expected error for %s", body)
		}
	}
}
