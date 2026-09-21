package executor

import (
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/openaicompat"
	"github.com/tidwall/gjson"
)

// upstreamResponseModelMaxLength bounds the persisted name. A malformed or
// hostile upstream can put an arbitrarily long string in the model field, and
// this value only ever flows into a log row and the panel.
const upstreamResponseModelMaxLength = 200

// upstreamResponseModelObserver captures the model name an upstream declares in
// its own response body, which is not always the model we asked it for: a
// provider may silently route an alias to a different build (a request for
// gemini-3.8-flash-high answered by gemini-3.8-flash-exp-a), or answer with an
// internal path name of its own.
//
// Observation is strictly read-only with respect to forwarding, logging and
// cost. The observed name never replaces the request-time model — see
// usageReporter.setModel for why an upstream echo must not win there — it is
// recorded beside it so the panel can surface the discrepancy for auditing.
type upstreamResponseModelObserver struct {
	mu sync.Mutex
	// first holds the earliest non-terminal declaration; terminal declarations
	// supersede it because they describe the model that actually produced the
	// completed answer. An OpenAI stream that opens as one model and completes as
	// another is exactly the case this audit exists to catch.
	first    string
	terminal string
}

// upstreamResponseModelProbes lists where each forwarded protocol declares its
// model, in priority order.
//
// The paths are mutually exclusive across the protocols we forward, so one
// protocol-agnostic probe covers every executor. That is deliberate: a
// per-provider dispatch table would have to be extended for every new upstream,
// and a provider missing from it would silently stop being audited.
var upstreamResponseModelProbes = []struct {
	path string
	// terminal marks declarations that supersede earlier ones outright.
	terminal bool
}{
	// Gemini and Antigravity repeat modelVersion on every chunk and have no
	// terminal event carrying it, so the latest declaration wins.
	{path: "modelVersion", terminal: true},
	{path: "response.modelVersion", terminal: true},
	{path: "response.response.modelVersion", terminal: true},
	// Anthropic declares the model once, inside message_start when streaming.
	{path: "message.model"},
	// OpenAI Responses API nests it; terminality comes from the event type.
	{path: "response.model"},
	// Anthropic non-streaming bodies, OpenAI chat completions chunks, Ollama.
	{path: "model"},
}

// upstreamResponseTerminalEventTypes are the OpenAI Responses events that carry
// the model of the finished response. Earlier events echo the requested model,
// so only these may override an established observation.
var upstreamResponseTerminalEventTypes = map[string]struct{}{
	"response.completed":  {},
	"response.done":       {},
	"response.failed":     {},
	"response.incomplete": {},
	"response.cancelled":  {},
	"response.canceled":   {},
}

// observeUpstreamResponse audits one upstream payload explicitly. Executors that
// hand appendOutputChunk a payload they synthesized must call this with the
// upstream's own bytes instead, so the audit records what the provider said
// rather than the model name we put into the translation.
func (r *usageReporter) observeUpstreamResponse(payload []byte) {
	if r == nil {
		return
	}
	r.upstreamResponse.observe(payload)
}

// observe records the model declared by one upstream chunk or response body.
// The payload may be a raw JSON body or a single SSE line; anything that is not
// a JSON object is ignored.
func (o *upstreamResponseModelObserver) observe(payload []byte) {
	if o == nil {
		return
	}
	body := jsonPayload(payload)
	if len(body) == 0 {
		return
	}
	root := openaicompat.ParseResponseRoot(body)
	for _, probe := range upstreamResponseModelProbes {
		value := root.Get(probe.path)
		if value.Type != gjson.String {
			continue
		}
		model := normalizeUpstreamResponseModel(value.String())
		if model == "" {
			continue
		}
		// Validate only after a candidate is found. Streaming deltas carry no
		// model at all, so the common path never pays for a full JSON scan, while
		// a payload that merely looks like it declares a model is still rejected.
		if !gjson.ValidBytes(body) {
			return
		}
		o.record(model, probe.terminal || isUpstreamResponseTerminalPayload(root))
		return
	}
}

func (o *upstreamResponseModelObserver) record(model string, terminal bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

// model returns the observed name, or "" when the upstream never declared one.
func (o *upstreamResponseModelObserver) model() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func isUpstreamResponseTerminalPayload(root gjson.Result) bool {
	eventType := strings.TrimSpace(root.Get("type").String())
	if eventType == "" {
		return false
	}
	_, ok := upstreamResponseTerminalEventTypes[eventType]
	return ok
}

func normalizeUpstreamResponseModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	runes := []rune(model)
	if len(runes) > upstreamResponseModelMaxLength {
		return string(runes[:upstreamResponseModelMaxLength])
	}
	return model
}
