package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Incremental Codex requests: one request's turn.
//
// Every Codex Responses call is compared with the previous turn of its
// conversation once its body is final (translated, sanitised, converged, with
// its prompt cache key), so the comparison sees exactly what goes upstream and
// every downstream protocol takes part. In shadow mode that comparison is all:
// the request goes out over HTTP as before and its reply becomes the next base.
// In "on" mode the request goes out over a websocket pinned to its chain; see
// codex_incremental_transport.go.

// codexIncrementalTurn is one request's part in incremental Codex requests.
type codexIncrementalTurn struct {
	pool     *codexIncrementalPool
	mode     string
	limits   codexIncrementalLimits
	maxFirst int
	identity codexIncrementalConnIdentity
	from     sdktranslator.Format
	cur      *codexIncrementalRequest
	chain    *codexIncrementalChain
	match    codexIncrementalMatch
	hit      bool
	agedOut  bool
	record   *codexIncrementalRecord

	// What the reply showed.
	responseID      string
	completed       bool
	failed          bool
	finalSeen       bool
	finalProjection codexIncrementalProjection
	finalUnusable   string
	observed        []codexIncrementalObservedItem

	// The websocket the turn went out on, if it did.
	conn          *codexIncrementalConn
	sink          *codexIncrementalTurnSink
	terminal      bool
	sentIncrement bool
	wsSent        int
	finished      bool
}

// codexIncrementalObservedItem is an output_item.done, kept in case the final
// event comes without its output. Hosted tool results (images) are not kept.
type codexIncrementalObservedItem struct {
	index    int64
	role     codexIncrementalItemRole
	itemType string
	raw      string
	call     codexIncrementalCall
}

func (e *CodexExecutor) incrementalPool() *codexIncrementalPool {
	if e != nil && e.incremental != nil {
		return e.incremental
	}
	return sharedCodexIncrementalPool()
}

// closeIncrementalSession frees the chains of a client websocket session that
// ended, without creating the pool when no request ever used it.
func (e *CodexExecutor) closeIncrementalSession(sessionID string) {
	if e == nil {
		return
	}
	pool := e.incremental
	if pool == nil {
		pool = codexIncrementalShared.Load()
	}
	pool.closeSession(sessionID)
}

func codexIncrementalLimitsFrom(cfg *config.Config) codexIncrementalLimits {
	return codexIncrementalLimits{
		idleTimeout:  cfg.CodexUpstreamIncrementalIdleTimeout(),
		maxConns:     cfg.CodexUpstreamIncrementalMaxChains(),
		maxConnsAuth: cfg.CodexUpstreamIncrementalMaxChainsPerAuth(),
	}
}

// beginCodexIncrementalTurn compares the body about to go upstream with the
// previous turn of its conversation. It returns nil when the feature is off.
func (e *CodexExecutor) beginCodexIncrementalTurn(execCtx *ExecutionContext, body []byte) *codexIncrementalTurn {
	if e == nil || execCtx == nil {
		return nil
	}
	mode := e.cfg.CodexUpstreamIncrementalMode()
	if mode == config.CodexUpstreamIncrementalOff {
		return nil
	}
	ctx := execCtx.Context
	apiKey := apiKeyFromContext(ctx)
	if mode == config.CodexUpstreamIncrementalOn && !e.cfg.CodexUpstreamIncrementalAllowsKey(apiKey) {
		mode = config.CodexUpstreamIncrementalShadow
	}
	turn := &codexIncrementalTurn{
		pool:     e.incrementalPool(),
		mode:     mode,
		limits:   codexIncrementalLimitsFrom(e.cfg),
		maxFirst: e.cfg.CodexUpstreamIncrementalMaxFirstRequestBytes(),
		from:     execCtx.SourceFormat,
		record:   &codexIncrementalRecord{Mode: mode, Result: "skip", Transport: "http", FullBytes: len(body), SentBytes: len(body)},
	}
	attachCodexIncrementalRecord(ctx, turn.record)

	auth := execCtx.Auth
	if !isCodexOAuthAdmissionAuth(auth) || strings.TrimSpace(auth.ID) == "" {
		// API key channels may front any Responses server; only the ChatGPT
		// backend's websocket is known to keep responses between turns.
		turn.skip("not_oauth_account")
		return turn
	}
	cur, reason := parseCodexIncrementalRequest(body)
	if reason != "" {
		turn.skip(reason)
		return turn
	}
	key := codexIncrementalConversationKey(execCtx, cur, apiKey)
	if key == "" {
		turn.skip("no_session_identity")
		return turn
	}
	turn.cur = cur
	token, _ := codexCreds(auth)
	tokenDigest := sha256.Sum256([]byte(token))
	turn.identity = codexIncrementalConnIdentity{
		authID:         auth.ID,
		promptCacheKey: cur.promptCacheKey,
		model:          execCtx.BaseModel,
		tokenDigest:    hex.EncodeToString(tokenDigest[:]),
		proxyURL:       resolveAuthProxyURL(e.cfg, auth),
	}
	turn.record.update(func(r *codexIncrementalRecord) { r.InputItems = len(cur.items) })
	defer func() {
		if turn.mode != config.CodexUpstreamIncrementalOn || turn.chain == nil {
			// Nothing will be built from the body: keep only what the next turn
			// compares, not a second copy of a multi-megabyte history.
			cur.dropContent()
		}
	}()

	chain, acquired, line := turn.pool.acquire(key, executionSessionIDFromOptions(execCtx.Options), auth.ID, cur, turn.limits)
	switch acquired {
	case codexChainBusy:
		turn.miss("conversation_busy")
		return turn
	case codexChainFull:
		turn.miss("chains_full")
		return turn
	}
	turn.chain = chain
	if acquired == codexChainNew {
		turn.miss(line.reason)
		if line.reason == "input_prefix_mismatch" {
			turn.record.update(func(r *codexIncrementalRecord) {
				r.PrevItems, r.MismatchAt = line.prevItems, line.mismatchAt
			})
		}
		return turn
	}
	match, reason := matchCodexIncremental(chain.base, cur)
	if reason == "" {
		reason = turn.transportReason(time.Now())
	}
	if reason != "" {
		turn.miss(reason)
		turn.noteEcho(reason, match)
		return turn
	}
	turn.match = match
	turn.hit = true
	turn.record.update(func(r *codexIncrementalRecord) {
		r.Result = "hit"
		r.NewItems = len(match.newItems)
		r.Echoed = match.echoed
		r.ChainTurn = chain.turns + 1
		r.SentBytes = codexIncrementalCreateSize(cur, match.newItems, chain.base.responseID)
	})
	return turn
}

// transportReason is what keeps a matching turn from continuing its chain on
// the wire: the connection that holds the base must still be there. Shadow mode
// has no connection and counts what would have happened to one.
func (t *codexIncrementalTurn) transportReason(now time.Time) string {
	if t.mode != config.CodexUpstreamIncrementalOn {
		if now.Sub(t.chain.startedAt) >= codexIncrementalConnMaxAge {
			t.agedOut = true
			return "connection_aged_out"
		}
		return ""
	}
	base := t.chain.base
	if base.connSerial == 0 {
		return "no_upstream_connection"
	}
	conn := t.chain.conn
	if conn == nil || conn.serial != base.connSerial || !conn.usable(t.identity, now) {
		return "upstream_connection_lost"
	}
	return ""
}

func (t *codexIncrementalTurn) skip(reason string) {
	t.record.update(func(r *codexIncrementalRecord) { r.Reason = reason })
}

func (t *codexIncrementalTurn) miss(reason string) {
	t.record.update(func(r *codexIncrementalRecord) {
		r.Result = "miss"
		r.Reason = reason
	})
}

// noteEcho records, for a turn that did not continue its chain because of what
// it sent back of the previous reply, the shape of that: how many items followed
// the previous input, how many of them were echoed output, and which part of
// the echo differed. Counts and part names only, never content.
func (t *codexIncrementalTurn) noteEcho(reason string, match codexIncrementalMatch) {
	switch reason {
	case "output_not_echoed":
		t.record.update(func(r *codexIncrementalRecord) { r.After = match.after })
	case "echoed_output_mismatch":
		t.record.update(func(r *codexIncrementalRecord) {
			r.After, r.Echoed = match.after, match.echoed
			if d := match.diff; d != nil {
				r.EchoDiff, r.EchoDiffAt = d.parts, d.at
				r.BaseCalls, r.EchoedCalls = d.baseCalls, d.echoedCalls
				r.BaseTextBytes, r.EchoedTextBytes = d.baseTextBytes, d.echoedTextBytes
			}
		})
	}
}

// codexIncrementalCreateSize is the size of the increment message, without
// building it.
func codexIncrementalCreateSize(cur *codexIncrementalRequest, newItems []codexIncrementalInputItem, previousResponseID string) int {
	size := len(cur.body) - (cur.inputEnd - cur.inputStart) + 2
	for i, item := range newItems {
		if i > 0 {
			size++
		}
		size += item.size()
	}
	// ,"previous_response_id":"…" and ,"type":"response.create"
	return size + len(previousResponseID) + 51
}

// codexIncrementalConversationKey names the conversation a request belongs to.
// The downstream caller is part of it, so two tenants or two keys never share a
// chain even if they send the same session identifier. So is the client thread,
// when the request names one: a request that names none keeps the key it had.
func codexIncrementalConversationKey(execCtx *ExecutionContext, cur *codexIncrementalRequest, apiKey string) string {
	session := ""
	switch {
	case executionSessionIDFromOptions(execCtx.Options) != "":
		session = "execution:" + executionSessionIDFromOptions(execCtx.Options)
	case cur.promptCacheKey != "":
		session = "prompt_cache_key:" + cur.promptCacheKey
	default:
		if seed := codexSessionPromptCacheSeed(execCtx.Options, execCtx.Request.Payload); seed != "" {
			session = "seed:" + seed
		}
	}
	if session == "" {
		return ""
	}
	tenant := cliproxyauth.NormalizedTenantID(metadataText(execCtx.Options.Metadata, cliproxyexecutor.TenantMetadataKey))
	keyDigest := sha256.Sum256([]byte(apiKey))
	parts := []string{"codex-incremental:v1", tenant, hex.EncodeToString(keyDigest[:]), execCtx.SourceFormat.String(), session}
	if thread := codexIncrementalThreadID(execCtx); thread != "" {
		parts = append(parts, "thread:"+thread)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// codexIncrementalThreadID is the client thread a request comes from, or "" when
// it names none.
//
// Codex runs every thread of a session, the root and each sub-agent it spawns,
// under the session's id: over HTTP all of them send the same prompt_cache_key,
// over Chat the key derived from the shared Session_id header is the same, and
// they share the opening of their history. Keyed on the session alone, a root
// and three sub-agents took turns on one key's chains and kept taking each
// other's (11-19 16:36–18:58, Codex Desktop over HTTP: 25.7% of sub-agent turns
// missed with input_prefix_mismatch, against 4.9% of root turns).
// The thread id is what tells them apart: the Thread-Id header, else the
// thread_id of the X-Codex-Turn-Metadata header, else, on the client websocket,
// where Codex sends its per-request headers in the body, the thread the
// request's client_metadata names.
//
// All of it is read from what the client sent. The body going upstream is no
// use here: fingerprint convergence has rewritten its client_metadata by then,
// and the session and full modes give every thread the same id.
func codexIncrementalThreadID(execCtx *ExecutionContext) string {
	if ginCtx := ginContextFrom(execCtx.Context); ginCtx != nil && ginCtx.Request != nil {
		if thread := strings.TrimSpace(ginCtx.Request.Header.Get("Thread-Id")); thread != "" {
			return thread
		}
		if thread := codexTurnMetadataThreadID(ginCtx.Request.Header.Get("X-Codex-Turn-Metadata")); thread != "" {
			return thread
		}
	}
	if executionSessionIDFromOptions(execCtx.Options) == "" {
		// Not the client websocket: the body is not read again for a field only
		// the websocket carries there.
		return ""
	}
	metadata := gjson.GetBytes(execCtx.Request.Payload, "client_metadata")
	if !metadata.IsObject() {
		return ""
	}
	if thread := strings.TrimSpace(metadata.Get("thread_id").String()); thread != "" {
		return thread
	}
	return codexTurnMetadataThreadID(metadata.Get("x-codex-turn-metadata").String())
}

// codexTurnMetadataThreadID reads thread_id out of Codex's turn metadata, a JSON
// object sent as a header over HTTP and as a string in client_metadata over the
// client websocket.
func codexTurnMetadataThreadID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, "thread_id").String())
}

// observe reads one event of the reply. Both transports pass every event
// through here, after the relay's own rewrites, so the projection is of what the
// client was given.
func (t *codexIncrementalTurn) observe(eventType string, payload []byte) {
	if t == nil || t.chain == nil {
		return
	}
	switch eventType {
	case "response.output_item.done":
		item := gjson.GetBytes(payload, "item")
		if !item.IsObject() {
			return
		}
		role, itemType := classifyCodexIncrementalItem(item.Raw)
		observed := codexIncrementalObservedItem{index: gjson.GetBytes(payload, "output_index").Int(), role: role, itemType: itemType}
		switch role {
		case codexItemText:
			observed.raw = item.Raw
		case codexItemCall:
			observed.call = codexIncrementalCallOf(item.Raw, itemType)
		}
		t.observed = append(t.observed, observed)
	case "response.completed":
		t.completed = true
		t.responseID = strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
		output := gjson.GetBytes(payload, "response.output")
		if output.IsArray() {
			items := output.Array()
			if len(items) > 0 {
				raws := make([]string, len(items))
				for i := range items {
					raws[i] = items[i].Raw
				}
				t.finalProjection, t.finalUnusable = projectCodexOutputItems(raws)
				t.finalSeen = true
			}
		}
	case "response.incomplete", "response.failed", "error":
		// A cut-short reply is still a reply for the client, but the next turn
		// would continue from a response the backend did not finish.
		t.failed = true
	}
}

// outputProjection is the projection of what the reply gave the client: the
// final event's output, or the output_item.done events when it had none.
func (t *codexIncrementalTurn) outputProjection() (codexIncrementalProjection, string) {
	if t.finalSeen {
		return t.finalProjection, t.finalUnusable
	}
	items := append([]codexIncrementalObservedItem(nil), t.observed...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
	text := sha256.New()
	textBytes := 0
	var calls []codexIncrementalCall
	for _, item := range items {
		switch item.role {
		case codexItemText:
			textBytes += writeCodexAssistantText(text, item.raw)
		case codexItemCall:
			calls = append(calls, item.call)
		case codexItemSkipped:
		default:
			itemType := item.itemType
			if itemType == "" {
				itemType = "unknown"
			}
			return codexIncrementalProjection{}, "output_item_unsupported:" + itemType
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), textBytes: textBytes, calls: calls}, ""
}

// finish settles the turn: a completed reply becomes the chain's next base,
// anything else leaves the chain without one. It runs once, after the reply has
// been read or abandoned.
func (t *codexIncrementalTurn) finish() {
	if t == nil || t.finished {
		return
	}
	t.finished = true
	defer t.pool.stats.add(t.record)
	if t.chain == nil {
		return
	}
	var base *codexIncrementalBase
	if t.completed && !t.failed && t.responseID != "" {
		projection, unusable := t.outputProjection()
		base = &codexIncrementalBase{
			items:      t.cur.itemDigests(),
			props:      t.cur.props,
			responseID: t.responseID,
			output:     projection,
			unusable:   unusable,
		}
		if t.conn != nil {
			base.connSerial = t.conn.serial
		}
	}
	outcome := codexIncrementalRelease{base: base}
	if t.conn != nil {
		t.conn.endTurn(t.sink)
		// Events of an unfinished reply are still on their way; the next turn
		// would read them as its own.
		outcome.dropConn = !t.terminal
	}
	if base != nil && t.hit && (t.mode != config.CodexUpstreamIncrementalOn || t.sentIncrement) {
		outcome.turns = t.chain.turns + 1
	}
	outcome.resetAge = t.agedOut
	t.pool.release(t.chain, outcome)
}
