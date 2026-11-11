package executor

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	"github.com/tidwall/gjson"
)

// The shape of a request the Codex upstream refused without saying why.
//
// chatgpt.com/backend-api/codex/responses sometimes answers 400 with
// {"detail":"Bad Request"} and nothing else: no code, no parameter, no message.
// Whatever the request did to earn it, the request is the only evidence, and the
// gateway keeps almost none of it. Bodies are stripped from the request log, the
// error log keeps a body only up to 1 MiB and rotates within minutes, and the
// upstream body goes out zstd-compressed and is never stored. A thread that
// fails for days leaves nothing to compare against the requests that succeed.
//
// So when that 400 arrives, the executor records the shape of the body it sent:
// sizes, counts by item type, how much of it is encrypted reasoning or images,
// what the gateway itself rewrote, and the identity headers that went out. It
// lands on the route trace step of the attempt (routetrace.Step), which the
// request log keeps in detail_content.diagnostic whatever the body retention
// setting says, and which the error log prints in its DIAGNOSTIC METADATA block.
//
// What the shape never holds: any text, id, call id, argument, URL, image,
// ciphertext or header value. Names (item types, tool types, roles, top-level
// keys) are the caller's own and only count when they fit a fixed alphabet or
// list (codex_request_shape_scan.go); identity ids are compared for equality and
// otherwise not recorded. The JSON is held to codexShapeBudgetBytes.
//
// It costs nothing on success: it is built only on the 400 path, after the
// upstream has answered. Building it is one pass over the plain body.
//
// Only the HTTP transport records it. The websocket transport reports upstream
// errors as events on a shared connection and has no attempt to attach one to.

// codexShapeBudgetBytes bounds the marshalled shape. A trace holds up to 32
// attempts and rides in every request log row of the request.
const codexShapeBudgetBytes = 2 << 10

type codexUpstreamShape struct {
	Bytes       codexShapeBytes             `json:"bytes"`
	Input       codexShapeInput             `json:"input"`
	Ciphertext  map[string]codexShapeCipher `json:"ciphertext,omitempty"`
	Tools       codexShapeTools             `json:"tools"`
	Images      codexShapeImages            `json:"images"`
	Request     codexShapeRequest           `json:"request"`
	Client      codexShapeClient            `json:"client"`
	Mutations   *codexBodyEdits             `json:"mutations,omitempty"`
	SentHeaders codexShapeHeaders           `json:"sent_headers"`

	// Scratch for scanBody.
	inputTypes    codexTypeCounter
	toolTypes     codexTypeCounter
	dataURLImages int
	lite          bool
}

type codexShapeBytes struct {
	// Plain is the JSON body; Zstd the bytes that went on the wire when it was
	// compressed, zero when it was not.
	Plain int `json:"plain"`
	Zstd  int `json:"zstd,omitempty"`
}

type codexShapeInput struct {
	Items  int            `json:"items"`
	ByType map[string]int `json:"by_type,omitempty"`
	// Bytes is the input array as it sits in the body; StringBytes is set instead
	// when input is a plain string.
	Bytes       int `json:"bytes,omitempty"`
	StringBytes int `json:"string_bytes,omitempty"`
	// The biggest single item, by type.
	LargestItemType  string `json:"largest_item_type,omitempty"`
	LargestItemBytes int    `json:"largest_item_bytes,omitempty"`
}

// codexShapeCipher is what the items carrying encrypted_content add up to.
type codexShapeCipher struct {
	N     int `json:"n"`
	Bytes int `json:"bytes"`
}

type codexShapeTools struct {
	TopLevel int            `json:"top_level"`
	ByType   map[string]int `json:"by_type,omitempty"`
	// Responses Lite carries its tools in input[] additional_tools items.
	AdditionalToolsItems int `json:"additional_tools_items,omitempty"`
	AdditionalToolsTools int `json:"additional_tools_tools,omitempty"`
}

type codexShapeImages struct {
	InputImage      int `json:"input_image"`
	InputImageBytes int `json:"input_image_bytes"`
	LargestBytes    int `json:"input_image_largest_bytes,omitempty"`
	// DataURLInText counts data:image/ URLs sitting inside text, not in an
	// input_image part.
	DataURLInText int `json:"data_url_in_text"`
}

type codexShapeRequest struct {
	// Keys are the top-level keys from a fixed list of the Responses API's;
	// OtherKeys counts the rest.
	Keys              []string `json:"keys,omitempty"`
	OtherKeys         int      `json:"other_keys,omitempty"`
	Store             *bool    `json:"store,omitempty"`
	Stream            *bool    `json:"stream,omitempty"`
	ReasoningEffort   bool     `json:"reasoning_effort"`
	ToolChoice        string   `json:"tool_choice,omitempty"`
	InstructionsBytes int      `json:"instructions_bytes,omitempty"`
}

// codexShapeClient is what the client said about the turn, from its
// X-Codex-Turn-Metadata header (the gateway rewrites some ids in the copy it
// forwards, not these fields).
type codexShapeClient struct {
	RequestKind           string            `json:"request_kind,omitempty"`
	WindowNumber          *int              `json:"window_number,omitempty"`
	HistoryInitialization string            `json:"history_initialization,omitempty"`
	TurnTrigger           string            `json:"turn_trigger,omitempty"`
	Source                string            `json:"source,omitempty"`
	ContextWindowAgeHours *float64          `json:"context_window_age_h,omitempty"`
	Extra                 map[string]string `json:"extra,omitempty"`
	// Lite says the client marked the request Responses Lite, by header or in
	// client_metadata.
	Lite bool `json:"lite,omitempty"`
}

// codexShapeHeaders is what went out on this attempt.
type codexShapeHeaders struct {
	Version         string `json:"version,omitempty"`
	UAVersion       string `json:"ua_version,omitempty"`
	WindowCounter   *int   `json:"window_counter,omitempty"`
	RoutingHint     bool   `json:"routing_hint"`
	ContentEncoding string `json:"content_encoding,omitempty"`
	LiteHeader      bool   `json:"lite_header"`
	// Whether the session id equals the thread, conversation and client request
	// ids. The ids are not recorded; present only when both sides were sent.
	SessionEqualsThread        *bool `json:"session_eq_thread,omitempty"`
	SessionEqualsConversation  *bool `json:"session_eq_conversation,omitempty"`
	SessionEqualsClientRequest *bool `json:"session_eq_client_request,omitempty"`
}

// codexShapeSource is what the shape is built from.
type codexShapeSource struct {
	// Plain is the JSON body as sent, before compression; with Request it gives
	// the sizes, and Request.Header the headers that went out.
	Plain   []byte
	Request *http.Request
	// ClientHeaders are the headers of the client's own request.
	ClientHeaders http.Header
	// Edits is what the gateway rewrote on the way; nil when nothing counted it.
	Edits *codexBodyEdits
	// Retried marks a body re-sent without its reasoning ciphertext.
	Retried bool
	Now     time.Time
}

func buildCodexUpstreamShape(src codexShapeSource) *codexUpstreamShape {
	shape := &codexUpstreamShape{}
	shape.scanBody(src.Plain)
	var sent http.Header
	if src.Request != nil {
		sent = src.Request.Header
		if strings.EqualFold(strings.TrimSpace(sent.Get("Content-Encoding")), "zstd") && src.Request.ContentLength > 0 {
			shape.Bytes.Zstd = int(src.Request.ContentLength)
		}
	}
	shape.SentHeaders = codexShapeSentHeaders(sent)
	shape.Client = codexShapeClientFrom(src.ClientHeaders, sent, src.Now)
	shape.Client.Lite = shape.lite || strings.EqualFold(strings.TrimSpace(src.ClientHeaders.Get(codexResponsesLiteHeader)), "true")

	var edits codexBodyEdits
	if src.Edits != nil {
		edits = *src.Edits
	}
	edits.ReasoningRetry = src.Retried
	if edits != (codexBodyEdits{}) {
		shape.Mutations = &edits
	}
	return shape
}

// marshalWithinBudget renders the shape, dropping the least useful detail until
// it fits codexShapeBudgetBytes. Ordinary requests fit as built; the cuts are
// for a request that uses every type, key and tool name the shape tracks.
func (s *codexUpstreamShape) marshalWithinBudget() json.RawMessage {
	cuts := []func(){
		func() {},
		func() {
			s.Input.ByType = topCodexCounts(s.Input.ByType, 12)
			s.Tools.ByType = topCodexCounts(s.Tools.ByType, 6)
		},
		func() { s.Request.Keys = nil },
		func() {
			s.Input.LargestItemType, s.Input.LargestItemBytes = "", 0
			s.Client.Extra = nil
		},
	}
	for _, cut := range cuts {
		cut()
		if raw, err := json.Marshal(s); err == nil && len(raw) <= codexShapeBudgetBytes {
			return raw
		}
	}
	// What is left when nothing else fits is still the two facts a comparison
	// starts from.
	raw, err := json.Marshal(struct {
		Bytes     codexShapeBytes `json:"bytes"`
		Items     int             `json:"items"`
		Mutations *codexBodyEdits `json:"mutations,omitempty"`
	}{s.Bytes, s.Input.Items, s.Mutations})
	if err != nil {
		return nil
	}
	return raw
}

// topCodexCounts keeps the n biggest buckets and folds the rest into "other".
func topCodexCounts(counts map[string]int, n int) map[string]int {
	if len(counts) <= n {
		return counts
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		if name != codexShapeOther {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	out := make(map[string]int, n+1)
	other := counts[codexShapeOther]
	for i, name := range names {
		if i < n {
			out[name] = counts[name]
		} else {
			other += counts[name]
		}
	}
	if other > 0 {
		out[codexShapeOther] = other
	}
	return out
}

// codexShapeSentHeaders reads what the attempt sent. Header values reach the
// shape only through the filters below: a version-like token, a counter, a known
// encoding, or whether two ids are equal.
func codexShapeSentHeaders(h http.Header) codexShapeHeaders {
	session := firstNonEmptyString(h.Get("Session-Id"), h.Get("Session_id"))
	return codexShapeHeaders{
		Version:         codexShapeVersion(h.Get("Version")),
		UAVersion:       codexShapeVersion(codexVersionFromUserAgent(h.Get("User-Agent"))),
		WindowCounter:   codexWindowCounter(h.Get("X-Codex-Window-Id")),
		RoutingHint:     strings.TrimSpace(h.Get("X-Codex-Routing-Hint")) != "",
		ContentEncoding: codexShapeEncoding(h.Get("Content-Encoding")),
		LiteHeader:      strings.TrimSpace(h.Get(codexResponsesLiteHeader)) != "",

		SessionEqualsThread:        codexEqualIfBoth(session, h.Get("Thread-Id")),
		SessionEqualsConversation:  codexEqualIfBoth(session, firstNonEmptyString(h.Get("Conversation_id"), h.Get("Conversation-Id"))),
		SessionEqualsClientRequest: codexEqualIfBoth(session, h.Get("X-Client-Request-Id")),
	}
}

func codexEqualIfBoth(a, b string) *bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return nil
	}
	equal := a == b
	return &equal
}

// codexShapeVersion keeps a version-like token (0.167.1, 0.167.1-alpha.3.2) and
// drops anything else.
func codexShapeVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '-', c == '_', c == '+':
		default:
			return ""
		}
	}
	return s
}

func codexShapeEncoding(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return ""
	case "zstd", "gzip", "br", "deflate", "identity":
		return s
	}
	return codexShapeOther
}

// codexWindowCounter reads the trailing counter of X-Codex-Window-Id
// ("<thread id>:<n>").
func codexWindowCounter(s string) *int {
	s = strings.TrimSpace(s)
	idx := strings.LastIndexByte(s, ':')
	if idx < 0 {
		return nil
	}
	n, err := strconv.Atoi(s[idx+1:])
	if err != nil || n < 0 || n > 1_000_000_000 {
		return nil
	}
	return &n
}

// codexShapeEnumValue keeps a client's enum-like metadata value (cold_resume,
// compaction, app-server) and turns anything that is not one into "other".
func codexShapeEnumValue(v gjson.Result) string {
	if v.Type != gjson.String {
		return ""
	}
	s := v.Str
	if s == "" || len(s) > codexShapeMaxNameBytes {
		return codexShapeOther
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == ':', c == '.', c == '-':
		default:
			return codexShapeOther
		}
	}
	return strings.Clone(s)
}

// codexShapeClientFrom reads the client's turn metadata: from the client's own
// request when it has the header, from what went out otherwise.
func codexShapeClientFrom(clientHeaders, sentHeaders http.Header, now time.Time) codexShapeClient {
	var client codexShapeClient
	raw := strings.TrimSpace(clientHeaders.Get("X-Codex-Turn-Metadata"))
	if raw == "" {
		raw = strings.TrimSpace(sentHeaders.Get("X-Codex-Turn-Metadata"))
	}
	meta := gjson.Parse(raw)
	if !meta.IsObject() {
		return client
	}
	meta.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "request_kind":
			client.RequestKind = codexShapeEnumValue(value)
		case "history_initialization":
			client.HistoryInitialization = codexShapeEnumValue(value)
		case "turn_trigger":
			client.TurnTrigger = codexShapeEnumValue(value)
		case "source":
			client.Source = codexShapeEnumValue(value)
		case "window_number":
			if value.Type == gjson.Number && value.Num >= 0 && value.Num <= 1_000_000_000 {
				n := int(value.Num)
				client.WindowNumber = &n
			}
		case "context_window_id":
			if value.Type == gjson.String {
				if age, ok := codexUUIDv7AgeHours(value.Str, now); ok {
					client.ContextWindowAgeHours = &age
				}
			}
		case "trigger", "reason", "implementation", "strategy":
			if enum := codexShapeEnumValue(value); enum != "" {
				if client.Extra == nil {
					client.Extra = make(map[string]string, 4)
				}
				client.Extra[key.Str] = enum
			}
		}
		return true
	})
	return client
}

// codexUUIDv7AgeHours is how long ago a UUIDv7 was minted, in hours to one
// decimal. The first 48 bits of a v7 id are Unix milliseconds. Anything that is
// not a v7 id, or lies in the future, has no age.
//
// Codex names a context window with one, so its age says how long the thread has
// been living on the same window.
func codexUUIDv7AgeHours(id string, now time.Time) (float64, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '7' {
		return 0, false
	}
	millis, err := strconv.ParseUint(id[:8]+id[9:13], 16, 64)
	if err != nil {
		return 0, false
	}
	hours := now.Sub(time.UnixMilli(int64(millis))).Hours()
	if hours < 0 {
		return 0, false
	}
	return math.Round(hours*10) / 10, true
}

// codexRefusalNamesACode reports whether a 400 body carries a machine-readable
// error code. Those 400s say what is wrong and are handled where they are read
// (invalid_encrypted_content retries without reasoning). The 400s this file is
// for are the ones that do not: {"detail":"Bad Request"}, a bare string, HTML,
// an error object whose code is null.
func codexRefusalNamesACode(answer []byte) bool {
	if len(answer) == 0 {
		return false
	}
	for _, path := range [...]string{"error.code", "code", "detail.code"} {
		if code := gjson.GetBytes(answer, path); code.Type == gjson.String && strings.TrimSpace(code.Str) != "" {
			return true
		}
	}
	return false
}

// recordCodexUnexplainedBadRequest attaches the shape of req to the route trace
// attempt that sent it, when the upstream answered 400 without an error code.
// answer is the start of the 400 body; retried marks a request that was re-sent
// without its reasoning ciphertext.
//
// It runs on the failure path only and must never make a failure worse, so a
// panic while reading a body of unknown shape is logged and swallowed.
func recordCodexUnexplainedBadRequest(execCtx *ExecutionContext, req *http.Request, plain, answer []byte, retried bool) {
	if execCtx == nil || req == nil || !isCodexChatGPTBackend(execCtx.Auth, req.URL) || codexRefusalNamesACode(answer) {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logWithRequestID(execCtx.Context).Warnf("codex executor: recording the request shape of an unexplained 400 panicked: %v", recovered)
		}
	}()
	var plainBytes, zstdBytes, items int
	attached := routetrace.AttachUpstreamRequestShape(execCtx.Context, func() json.RawMessage {
		shape := buildCodexUpstreamShape(codexShapeSource{
			Plain:         plain,
			Request:       req,
			ClientHeaders: codexAdmissionHeadersFromContext(execCtx.Context),
			Edits:         codexBodyEditsFromContext(execCtx.Context),
			Retried:       retried,
			Now:           time.Now(),
		})
		plainBytes, zstdBytes, items = shape.Bytes.Plain, shape.Bytes.Zstd, shape.Input.Items
		return shape.marshalWithinBudget()
	})
	if attached {
		logWithRequestID(execCtx.Context).Infof("codex executor: upstream answered 400 without an error code; the request shape is in the request log (plain=%d zstd=%d input_items=%d reasoning_retry=%t)", plainBytes, zstdBytes, items, retried)
	}
}
