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
	callKey  string
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

// codexIncrementalCreateSize is the size of the increment message, without
// building it.
func codexIncrementalCreateSize(cur *codexIncrementalRequest, newItems []codexIncrementalInputItem, previousResponseID string) int {
	size := len(cur.body) - (cur.inputEnd - cur.inputStart) + 2
	for i, item := range newItems {
		if i > 0 {
			size++
		}
		size += len(item.raw)
	}
	// ,"previous_response_id":"…" and ,"type":"response.create"
	return size + len(previousResponseID) + 51
}

// codexIncrementalConversationKey names the conversation a request belongs to.
// The downstream caller is part of it, so two tenants or two keys never share a
// chain even if they send the same session identifier.
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
	sum := sha256.Sum256([]byte(strings.Join([]string{"codex-incremental:v1", tenant, hex.EncodeToString(keyDigest[:]), execCtx.SourceFormat.String(), session}, "\x00")))
	return hex.EncodeToString(sum[:16])
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
			observed.callKey = codexIncrementalCallKey(item.Raw, itemType)
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
	var calls []string
	for _, item := range items {
		switch item.role {
		case codexItemText:
			writeCodexAssistantText(text, item.raw)
		case codexItemCall:
			calls = append(calls, item.callKey)
		case codexItemSkipped:
		default:
			itemType := item.itemType
			if itemType == "" {
				itemType = "unknown"
			}
			return codexIncrementalProjection{}, "output_item_unsupported:" + itemType
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), calls: calls}, ""
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
