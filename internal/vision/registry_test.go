package vision

import (
	"context"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type fakeAnalyzer struct {
	resp AnalyzeResponse
	err  error
}

func (f fakeAnalyzer) Analyze(context.Context, AnalyzeRequest) (AnalyzeResponse, error) {
	return f.resp, f.err
}

func (f fakeAnalyzer) Name() string { return "fake" }

func TestComputeHash(t *testing.T) {
	h1 := ComputeHash("same-data")
	h2 := ComputeHash("same-data")
	h3 := ComputeHash("different-data")

	if h1 != h2 {
		t.Fatal("same data should produce same hash")
	}
	if h1 == h3 {
		t.Fatal("different data should produce different hash")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char hex hash, got %d", len(h1))
	}
}

func TestSessionStoreGetOrCreateEntry(t *testing.T) {
	s := &SessionStore{entries: make(map[ImageHash]*ImageEntry)}
	h1 := ComputeHash("img1")
	h2 := ComputeHash("img2")

	e1 := s.GetOrCreateEntry(h1, 1, 10)
	if e1.StableOrdinal != 1 {
		t.Fatalf("expected ordinal 1, got %d", e1.StableOrdinal)
	}

	// Same hash returns existing entry
	e1again := s.GetOrCreateEntry(h1, 1, 10)
	if e1again != e1 {
		t.Fatal("expected same entry for same hash")
	}

	e2 := s.GetOrCreateEntry(h2, 2, 10)
	if e2.StableOrdinal != 2 {
		t.Fatalf("expected ordinal 2, got %d", e2.StableOrdinal)
	}

	all := s.AllEntries()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
}

func TestSessionStoreUpdateEntry(t *testing.T) {
	s := &SessionStore{entries: make(map[ImageHash]*ImageEntry)}
	h := ComputeHash("test")
	s.GetOrCreateEntry(h, 1, 10)

	s.UpdateEntry(h, func(e *ImageEntry) {
		e.Summary.Summary = "test summary"
		e.Revision = 1
	})

	e := s.GetEntry(h)
	if e.Summary.Summary != "test summary" {
		t.Fatalf("summary = %q, want test summary", e.Summary.Summary)
	}
	if e.Revision != 2 {
		t.Fatalf("revision = %d, want 2 (fn sets 1, then ++)", e.Revision)
	}
}

func TestLRUEviction(t *testing.T) {
	s := &SessionStore{entries: make(map[ImageHash]*ImageEntry)}
	// Create 5 entries with max 3
	h := make([]ImageHash, 5)
	for i := 0; i < 5; i++ {
		h[i] = ComputeHash(string(rune('0' + i)))
		s.GetOrCreateEntry(h[i], i+1, 3)
	}

	// Only most recent 3 should survive
	all := s.AllEntries()
	if len(all) != 3 {
		t.Fatalf("expected 3 entries (LRU evicted 2), got %d", len(all))
	}

	// After evicting, ordinals should re-distribute
	for _, e := range all {
		if e.StableOrdinal < 3 {
			t.Fatalf("entry with ordinal %d should have been evicted", e.StableOrdinal)
		}
	}
}

func TestGlobalRegistrySessionGC(t *testing.T) {
	r := newGlobalRegistry(GlobalConfig{
		MaxSessions:          100,
		MaxEntriesPerSession: 10,
		SessionTTL:           10 * time.Millisecond,
	})

	s1 := r.GetOrCreateSession("session-1")
	s2 := r.GetOrCreateSession("session-2")
	if r.GetSession("session-1") == nil {
		t.Fatal("expected session-1 to exist")
	}

	// Touch session-1 to keep it alive
	_ = s1
	_ = s2

	// Wait for TTL to expire
	time.Sleep(20 * time.Millisecond)
	r.gc()

	if r.GetSession("session-1") != nil {
		t.Fatal("expected session-1 to be GC'd after TTL")
	}
}

func TestGlobalEvictOldestSession(t *testing.T) {
	r := newGlobalRegistry(GlobalConfig{
		MaxSessions: 2,
	})
	r.GetOrCreateSession("sess-a")
	r.GetOrCreateSession("sess-b")

	// Create C, which should evict the oldest (A)
	r.GetOrCreateSession("sess-c")

	sessions, _ := r.Stats()
	if sessions != 2 {
		t.Fatalf("expected 2 sessions after eviction, got %d", sessions)
	}

	// Only B and C should exist
	if r.GetSession("sess-a") != nil {
		t.Fatal("expected sess-a to be evicted")
	}
	if r.GetSession("sess-b") == nil {
		t.Fatal("expected sess-b to exist")
	}
}

func TestWalkPayloadMessages(t *testing.T) {
	payload := []byte(`{
		"model": "deepseek-v4-flash",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]},
			{"role": "assistant", "content": "hi"},
			{"role": "user", "content": [{"type": "text", "text": "what is this?"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}]}
		]
	}`)

	walk := WalkPayload(payload)
	if len(walk.Parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(walk.Parts))
	}
	if !walk.Parts[0].IsCurrent {
		t.Fatal("expected image to be current (last user message)")
	}
	if walk.Parts[0].Data != "iVBORw0KGgo=" {
		t.Fatalf("data = %q, want iVBORw0KGgo=", walk.Parts[0].Data)
	}
}

func TestWalkPayloadResponses(t *testing.T) {
	payload := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "hello"}]},
			{"role": "assistant", "content": [{"type": "output_text", "text": "hi"}]},
			{"role": "user", "content": [{"type": "input_image", "image_url": "data:image/png;base64,iVBOR="}]}
		]
	}`)

	walk := WalkPayload(payload)
	if len(walk.Parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(walk.Parts))
	}
	if !walk.Parts[0].IsCurrent {
		t.Fatal("expected image to be current")
	}
	if walk.Parts[0].Data != "iVBOR=" {
		t.Fatalf("data = %q, want iVBOR=", walk.Parts[0].Data)
	}
}

func TestWalkPayloadResponsesFunctionCallOutputCurrentImage(t *testing.T) {
	payload := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "inspect this screenshot"}]},
			{"type": "function_call", "call_id": "call_1", "name": "get_app_state", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": [{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="}]}
		]
	}`)

	walk := WalkPayload(payload)
	if len(walk.Parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(walk.Parts))
	}
	if !walk.Parts[0].IsCurrent {
		t.Fatal("expected function_call_output image after latest user to be current")
	}
	if !walk.CurrentImages || walk.HistoricalOnly {
		t.Fatalf("current flags = CurrentImages:%v HistoricalOnly:%v, want current only", walk.CurrentImages, walk.HistoricalOnly)
	}
	if walk.Parts[0].Path != "input.2.output.0" {
		t.Fatalf("path = %q, want input.2.output.0", walk.Parts[0].Path)
	}
	if walk.Parts[0].Data != "iVBORw0KGgo=" {
		t.Fatalf("data = %q, want iVBORw0KGgo=", walk.Parts[0].Data)
	}
}

func TestWalkPayloadResponsesFunctionCallOutputBeforeLatestUserIsHistorical(t *testing.T) {
	payload := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "inspect this screenshot"}]},
			{"type": "function_call_output", "call_id": "call_1", "output": [{"type": "input_image", "image_url": "data:image/png;base64,oldImage="}]},
			{"role": "user", "content": [{"type": "input_text", "text": "now answer a normal text question"}]}
		]
	}`)

	walk := WalkPayload(payload)
	if len(walk.Parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(walk.Parts))
	}
	if walk.Parts[0].IsCurrent {
		t.Fatal("expected function_call_output image before latest user to be historical")
	}
	if !walk.HistoricalOnly {
		t.Fatal("expected HistoricalOnly to be true")
	}
}

func TestWalkPayloadHistorical(t *testing.T) {
	payload := []byte(`{
		"model": "deepseek-v4-flash",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,oldImage1="}}]},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "what about that image?"}
		]
	}`)

	walk := WalkPayload(payload)
	if len(walk.Parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(walk.Parts))
	}
	if walk.Parts[0].IsCurrent {
		t.Fatal("expected image to be historical (not in last user message)")
	}
	if !walk.HistoricalOnly {
		t.Fatal("expected HistoricalOnly to be true")
	}
}

func TestReplaceImagePart(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc="}}]}]}`)
	ip := ImagePart{ArrayName: "messages", MsgIdx: 0, PartIdx: 0}

	modified, err := ReplaceImagePart(payload, ip, "[Image #1: test]")
	if err != nil {
		t.Fatalf("ReplaceImagePart failed: %v", err)
	}

	// Verify the part was replaced with text
	walk := WalkPayload(modified)
	if len(walk.Parts) != 0 {
		t.Fatal("expected no image parts after replacement")
	}
}

func TestInjectRegistryNote(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"}]}]}`)

	modified, err := InjectRegistryNote(payload, "[Image Registry]\nImage #1: test description")
	if err != nil {
		t.Fatalf("InjectRegistryNote failed: %v", err)
	}

	walk := WalkPayload(modified)
	if len(walk.Parts) != 0 {
		t.Fatal("note injection should not add image parts")
	}
}

func TestDetectIntent(t *testing.T) {
	tests := []struct {
		msg   string
		count int
		want  Intent
	}{
		{"好的", 2, IntentNone},
		{"继续", 2, IntentNone},
		{"Image #1 里面有什么", 3, IntentFollowUp},
		{"第一张图里那个按钮是什么", 3, IntentFollowUp},
		{"右边那个弹窗是什么", 2, IntentFollowUp},
		{"第一张和第二张有什么差异", 3, IntentFollowUp},
		{"", 2, IntentNone},
	}

	for _, tt := range tests {
		got := DetectIntent(tt.msg, tt.count)
		if got != tt.want {
			t.Errorf("DetectIntent(%q, %d) = %d, want %d", tt.msg, tt.count, got, tt.want)
		}
	}
}

func TestAmbiguityDetection(t *testing.T) {
	// Multiple historical images + vague question = ambiguous
	msg := "还有什么细节"
	intent := DetectIntent(msg, 3)
	if intent != IntentAmbiguous {
		t.Fatalf("expected IntentAmbiguous for '还有什么细节' with 3 images, got %d", intent)
	}

	// Same question but only 2 images = follow-up (can be specific enough)
	intent = DetectIntent(msg, 2)
	if intent != IntentFollowUp {
		t.Fatalf("expected IntentFollowUp for '还有什么细节' with 2 images, got %d", intent)
	}
}

func TestExtractImageReference(t *testing.T) {
	tests := []struct {
		msg  string
		want int
	}{
		{"Image #1 是什么", 1},
		{"看看 image #2 吧", 2},
		{"第一张图里有什么", 1},
		{"第三张图里的按钮", 3},
		{"随便看看", 0},
		{"没有引用", 0},
	}

	for _, tt := range tests {
		got := ExtractImageReference(tt.msg)
		if got != tt.want {
			t.Errorf("ExtractImageReference(%q) = %d, want %d", tt.msg, got, tt.want)
		}
	}
}

func TestBuildShortPlaceholder(t *testing.T) {
	e := &ImageEntry{StableOrdinal: 2}
	p := BuildShortPlaceholder(e)
	if p != "[Image #2 from previous turn]" {
		t.Fatalf("placeholder = %q, want [Image #2 from previous turn]", p)
	}
}

func TestBuildRegistryNote(t *testing.T) {
	entries := []*ImageEntry{
		{StableOrdinal: 1, Summary: ImageSummary{Summary: "UI design screenshot"}},
		{StableOrdinal: 2, Summary: ImageSummary{Summary: "Error log terminal"}},
	}

	note := BuildRegistryNote(entries, 0)
	if note == "" {
		t.Fatal("expected non-empty registry note")
	}
	if len(note) < 50 {
		t.Fatalf("note too short: %q", note)
	}
}

func TestBuildRegistryNoteTargeted(t *testing.T) {
	entries := []*ImageEntry{
		{StableOrdinal: 1, Summary: ImageSummary{Summary: "UI design"}},
		{StableOrdinal: 2, Summary: ImageSummary{Summary: "Error log"}},
	}

	// Target ordinal 2
	note := BuildRegistryNote(entries, 2)
	if len(note) > 250 {
		t.Fatalf("targeted note too long: got %d chars", len(note))
	}
}

func TestBuildAmbiguityNote(t *testing.T) {
	entries := []*ImageEntry{
		{StableOrdinal: 1, Summary: ImageSummary{}},
		{StableOrdinal: 2, Summary: ImageSummary{}},
		{StableOrdinal: 3, Summary: ImageSummary{}},
	}

	note := BuildAmbiguityNote(entries)
	if len(note) < 60 {
		t.Fatalf("expected ambiguity note to mention 3 images, got %q", note)
	}
}

func TestSessionKeyResolution(t *testing.T) {
	// No session key available
	auth := &cliproxyauth.Auth{ID: "test-auth", Attributes: map[string]string{}}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{}}

	key, ok := ResolveSessionKey(opts, auth)
	if ok {
		t.Fatal("expected false when no session key available")
	}
	if key != "" {
		t.Fatal("expected empty key when no session available")
	}
}

func TestComputeImageHash(t *testing.T) {
	h1 := ComputeHash("data")
	h2 := ComputeHash("data2")
	if h1 == h2 {
		t.Fatal("different data should produce different hashes")
	}
}

func TestProcessorHistoricalReanalysisKeepsReachableState(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry: registry,
		analyzer: fakeAnalyzer{resp: AnalyzeResponse{
			Summary: ImageSummary{
				Summary:  "reanalyzed",
				OCRHints: []string{"ocr2"},
			},
		}},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"第一张图里有什么？"}]}`)

	res, err := processor.Process(context.Background(), payload, SessionKey("sess-1"), 1)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !strings.Contains(res.RegistryNote, "Image #1: reanalyzed") {
		t.Fatalf("registry note = %q, want reanalyzed summary", res.RegistryNote)
	}
	if strings.Contains(res.RegistryNote, "未出现在当前请求") {
		t.Fatalf("registry note should not claim image is unavailable: %q", res.RegistryNote)
	}

	entries := registry.GetSession("sess-1").AllEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].CurrentPayloadReachable {
		t.Fatal("historical image present in payload should be marked reachable")
	}
}

func TestProcessorWithoutSessionAssignsDistinctEphemeralOrdinals(t *testing.T) {
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"b"},{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]},{"role":"assistant","content":"ok2"},{"role":"user","content":"继续"}]}`)

	res, err := processor.Process(context.Background(), payload, "", 1)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	out := string(res.Payload)
	if strings.Count(out, "[Image #1 from previous turn]") != 1 {
		t.Fatalf("payload should contain exactly one Image #1 placeholder: %s", out)
	}
	if strings.Count(out, "[Image #2 from previous turn]") != 1 {
		t.Fatalf("payload should contain exactly one Image #2 placeholder: %s", out)
	}
}

func TestProcessorDuplicateCurrentImageDoesNotAdvanceOrdinal(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry:   registry,
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)

	if _, err := processor.Process(context.Background(), payload, SessionKey("sess-dup"), 1); err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
	if _, err := processor.Process(context.Background(), payload, SessionKey("sess-dup"), 2); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}

	store := registry.GetSession("sess-dup")
	if store == nil {
		t.Fatal("expected session store to exist")
	}
	if store.nextOrdinal != 1 {
		t.Fatalf("nextOrdinal = %d, want 1 for repeated same image", store.nextOrdinal)
	}
	if len(store.AllEntries()) != 1 {
		t.Fatalf("expected 1 entry for repeated same image, got %d", len(store.AllEntries()))
	}
}

func TestProcessorResetsReachabilityPerRequest(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry:   registry,
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload1 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	payload2 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]}`)
	payload3 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first again"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"还有什么细节？"}]}`)

	if _, err := processor.Process(context.Background(), payload1, SessionKey("sess-reset"), 1); err != nil {
		t.Fatalf("payload1 Process() error = %v", err)
	}
	if _, err := processor.Process(context.Background(), payload2, SessionKey("sess-reset"), 2); err != nil {
		t.Fatalf("payload2 Process() error = %v", err)
	}
	res, err := processor.Process(context.Background(), payload3, SessionKey("sess-reset"), 3)
	if err != nil {
		t.Fatalf("payload3 Process() error = %v", err)
	}

	if strings.Count(res.RegistryNote, "未出现在当前请求") != 1 {
		t.Fatalf("registry note should mark exactly one older image unavailable: %q", res.RegistryNote)
	}
}

func TestProcessorPhase2NoImagesInPayload(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry:   registry,
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	// First request: establish an image in the session store
	payload1 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)
	res1, err := processor.Process(context.Background(), payload1, SessionKey("sess-phase2"), 1)
	if err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
	if res1.ImagesFound != 1 {
		t.Fatalf("expected 1 image in first request, got %d", res1.ImagesFound)
	}

	// Manually seed a summary so Phase 2 has something to inject
	store := registry.GetSession("sess-phase2")
	if store == nil {
		t.Fatal("expected session store to exist")
	}
	store.UpdateEntry(ComputeHash("QUJD"), func(e *ImageEntry) {
		e.Summary = ImageSummary{
			Summary:    "Test UI design screenshot",
			OCRHints:   []string{"Button: Submit", "Title: Welcome"},
			Confidence: "high",
		}
	})

	// Second request: pure text, no images -- Phase 2 should fire
	payload2 := []byte(`{"messages":[{"role":"user","content":"Image #1 what was in it?"}]}`)
	res2, err := processor.Process(context.Background(), payload2, SessionKey("sess-phase2"), 2)
	if err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if res2.ImagesFound != 0 {
		t.Fatalf("expected 0 images in second request, got %d", res2.ImagesFound)
	}
	if res2.RegistryNote == "" {
		t.Fatal("Phase 2 should inject a registry note even when payload has no image parts")
	}
	if !strings.Contains(res2.RegistryNote, "Image #1") {
		t.Fatalf("registry note should reference Image #1: %q", res2.RegistryNote)
	}
}

func TestProcessorPhase2ExplicitReferenceNoImageData(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry:   registry,
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload1 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"check this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,WFla"}}]}]}`)
	if _, err := processor.Process(context.Background(), payload1, SessionKey("sess-ref"), 1); err != nil {
		t.Fatalf("first Process() error = %v", err)
	}

	store := registry.GetSession("sess-ref")
	store.UpdateEntry(ComputeHash("WFla"), func(e *ImageEntry) {
		e.Summary = ImageSummary{
			Summary:    "Error log: connection timeout",
			OCRHints:   []string{"Error: 503", "Service Unavailable"},
			Confidence: "high",
		}
	})

	// Explicit "Image #1" reference, no images in payload
	payload2 := []byte(`{"messages":[{"role":"user","content":"Image #1 what error?"}]}`)
	res2, err := processor.Process(context.Background(), payload2, SessionKey("sess-ref"), 2)
	if err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if !strings.Contains(res2.RegistryNote, "Image #1") {
		t.Fatalf("registry note should reference Image #1: %q", res2.RegistryNote)
	}
	if !strings.Contains(res2.RegistryNote, "connection timeout") {
		t.Fatalf("registry note should include cached summary: %q", res2.RegistryNote)
	}
}

func TestProcessorPhase2AmbiguityWithoutImages(t *testing.T) {
	registry := newGlobalRegistry(DefaultGlobalConfig())
	processor := &Processor{
		registry:   registry,
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	p1 := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}}]}]}`)
	if _, err := processor.Process(context.Background(), p1, SessionKey("sess-ambig"), 1); err != nil {
		t.Fatal(err)
	}
	p2 := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QkJC"}}]}]}`)
	if _, err := processor.Process(context.Background(), p2, SessionKey("sess-ambig"), 2); err != nil {
		t.Fatal(err)
	}
	p3 := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Q0ND"}}]}]}`)
	if _, err := processor.Process(context.Background(), p3, SessionKey("sess-ambig"), 3); err != nil {
		t.Fatal(err)
	}

	store := registry.GetSession("sess-ambig")
	store.UpdateEntry(ComputeHash("QUFB"), func(e *ImageEntry) {
		e.Summary = ImageSummary{Summary: "First image", Confidence: "high"}
	})
	store.UpdateEntry(ComputeHash("QkJC"), func(e *ImageEntry) {
		e.Summary = ImageSummary{Summary: "Second image", Confidence: "high"}
	})
	store.UpdateEntry(ComputeHash("Q0ND"), func(e *ImageEntry) {
		e.Summary = ImageSummary{Summary: "Third image", Confidence: "high"}
	})

	// Ambiguous question with 3+ cached images, no payload images
	p4 := []byte(`{"messages":[{"role":"user","content":"detail?"}]}`)
	res3, err := processor.Process(context.Background(), p4, SessionKey("sess-ambig"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res3.RegistryNote, "未明确指向") {
		t.Fatalf("Phase 2 should inject ambiguity note: %q", res3.RegistryNote)
	}
}

func TestProcessorPhase2DoesNotFireWhenNoSession(t *testing.T) {
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	res, err := processor.Process(context.Background(), payload, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.RegistryNote != "" {
		t.Fatalf("expected no registry note without session, got: %q", res.RegistryNote)
	}
	if string(res.Payload) != string(payload) {
		t.Fatal("payload should be unchanged without session")
	}
}

func TestA3ProcessCurrentTurnReplacesImage(t *testing.T) {
	a3Resp := AnalyzeResponse{
		Summary: ImageSummary{
			Summary:    "A login form with username and password fields",
			OCRHints:   []string{"Username:", "Password:"},
			Confidence: "high",
		},
	}
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{resp: a3Resp},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)

	modified, err := processor.A3ProcessCurrentTurn(context.Background(), payload, SessionKey("sess-a3"), 1)
	if err != nil {
		t.Fatalf("A3ProcessCurrentTurn() error = %v", err)
	}

	walk := WalkPayload(modified)
	if len(walk.Parts) > 0 {
		t.Fatal("A3 should replace all current-turn images, leaving no image parts")
	}

	modifiedStr := string(modified)
	if !strings.Contains(modifiedStr, "login form") {
		t.Fatalf("modified payload should contain analyzer summary: %s", modifiedStr)
	}
	if !strings.Contains(modifiedStr, "未直接查看原图") {
		t.Fatalf("modified payload should contain degradation note: %s", modifiedStr)
	}
}

func TestA3ProcessCurrentTurnWritesToRegistry(t *testing.T) {
	a3Resp := AnalyzeResponse{
		Summary: ImageSummary{
			Summary:  "Code editor showing syntax highlighting",
			OCRHints: []string{"function main()"},
		},
	}
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{resp: a3Resp},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Y29kZQ=="}}]}]}`)

	if _, err := processor.A3ProcessCurrentTurn(context.Background(), payload, SessionKey("sess-a3-store"), 1); err != nil {
		t.Fatalf("A3ProcessCurrentTurn() error = %v", err)
	}

	store := processor.registry.GetSession("sess-a3-store")
	if store == nil {
		t.Fatal("expected session store to exist")
	}
	entries := store.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry in session store, got %d", len(entries))
	}
	if entries[0].Summary.Summary != "Code editor showing syntax highlighting" {
		t.Fatalf("expected cached summary, got: %q", entries[0].Summary.Summary)
	}
	if !entries[0].CurrentPayloadReachable {
		t.Fatal("A3-processed image should be marked as reachable")
	}
}

func TestA3ProcessCurrentTurnRemoteImage(t *testing.T) {
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`)

	modified, err := processor.A3ProcessCurrentTurn(context.Background(), payload, SessionKey("sess-remote"), 1)
	if err != nil {
		t.Fatalf("A3ProcessCurrentTurn() error = %v", err)
	}

	modifiedStr := string(modified)
	if strings.Contains(modifiedStr, "https://example.com") {
		t.Fatalf("remote URL should be removed: %s", modifiedStr)
	}
	if !strings.Contains(modifiedStr, "暂不支持远程") {
		t.Fatalf("should contain remote image note: %s", modifiedStr)
	}
}

func TestA3WithNoSessionStillReplacesImage(t *testing.T) {
	processor := &Processor{
		registry:   newGlobalRegistry(DefaultGlobalConfig()),
		analyzer:   fakeAnalyzer{resp: AnalyzeResponse{Summary: ImageSummary{Summary: "terminal output"}}},
		maxEntries: DefaultGlobalConfig().MaxEntriesPerSession,
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVybQ=="}}]}]}`)

	modified, err := processor.A3ProcessCurrentTurn(context.Background(), payload, "", 1)
	if err != nil {
		t.Fatalf("A3ProcessCurrentTurn() error = %v", err)
	}

	walk := WalkPayload(modified)
	if len(walk.Parts) > 0 {
		t.Fatal("A3 should replace images even without a session key")
	}
}
