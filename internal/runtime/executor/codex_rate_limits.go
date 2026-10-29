package executor

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Codex rate-limit telemetry.
//
// The ChatGPT backend states how much of the account's plan windows is used on
// every answer: as x-codex-primary-* and x-codex-secondary-* response headers on
// HTTP, and as a `codex.rate_limits` event on the stream (the only place a
// websocket connection gets it). This file reads both and passes the windows to
// the scheduler's observer, which parks the account the moment a window is full
// and keeps the console's quota card current. Nothing here decides anything:
// reading is the executor's part, routing stays the scheduler's.
//
// Only ChatGPT OAuth accounts talking to chatgpt.com are read. Whatever a relay
// or an API-key upstream sends under these names describes someone else's
// allowance, and acting on it would park an account for a window it does not own.
// Only the account's main limit is read, the unprefixed x-codex-* names. Limits
// metered on their own (a model with a separate allowance) carry their own
// prefix, and using one up says nothing about the rest of the account.

const (
	// codexRateLimitsEventType is the stream event carrying the same numbers.
	codexRateLimitsEventType = "codex.rate_limits"

	// codexRateLimitMaxResetHorizon is how far ahead a reset is believed; it
	// matches the scheduler's own bound (observedQuotaMaxHorizon). Plan windows are
	// hours to a week long, so anything past this is a unit mistake or garbage, and
	// is treated as no reset at all instead of a reason to park an account.
	codexRateLimitMaxResetHorizon = 35 * 24 * time.Hour

	// codexRateLimitMaxWindowMinutes bounds a believable window length (a year).
	codexRateLimitMaxWindowMinutes = 366 * 24 * 60

	// codexRateLimitMaxEventLine bounds how much of one stream line is kept to look
	// for an event. The event is a few hundred bytes; everything longer is content.
	codexRateLimitMaxEventLine = 8 << 10

	// codexMainRateLimitName is the id of the account's main limit, which is what
	// an event without a limit name describes.
	codexMainRateLimitName = "codex"
)

var codexRateLimitsEventMarker = []byte(codexRateLimitsEventType)

// codexRateLimitWindowInput is a window as read off the wire, before it is
// checked.
type codexRateLimitWindowInput struct {
	usedPercent     float64
	hasUsed         bool
	windowMinutes   int64
	resetAt         int64
	resetAfterSecs  int64
	hasResetAfter   bool
	hasWindowMinute bool
}

// newCodexRateLimitWindow validates one window. It returns false for a window the
// upstream sent no usable usage figure for, or one that carries no data at all
// (the official client drops a zero percent with no length and no reset too).
func newCodexRateLimitWindow(in codexRateLimitWindowInput, now time.Time) (cliproxyexecutor.QuotaWindowObservation, bool) {
	if !in.hasUsed || math.IsNaN(in.usedPercent) || math.IsInf(in.usedPercent, 0) || in.usedPercent < 0 {
		return cliproxyexecutor.QuotaWindowObservation{}, false
	}
	window := cliproxyexecutor.QuotaWindowObservation{UsedPercent: math.Min(in.usedPercent, 100)}
	if in.hasWindowMinute && in.windowMinutes > 0 && in.windowMinutes <= codexRateLimitMaxWindowMinutes {
		window.WindowMinutes = int(in.windowMinutes)
		window.Label = codexQuotaWindowLabel(window.WindowMinutes)
	}
	horizon := now.Add(codexRateLimitMaxResetHorizon)
	switch {
	case in.resetAt > 0:
		if resetAt := time.Unix(in.resetAt, 0); !resetAt.After(horizon) {
			window.ResetAt = resetAt
		}
	case in.hasResetAfter && in.resetAfterSecs > 0 && in.resetAfterSecs <= int64(codexRateLimitMaxResetHorizon/time.Second):
		window.ResetAt = now.Add(time.Duration(in.resetAfterSecs) * time.Second)
	}
	if window.UsedPercent == 0 && window.WindowMinutes == 0 && window.ResetAt.IsZero() {
		return cliproxyexecutor.QuotaWindowObservation{}, false
	}
	return window, true
}

// parseCodexRateLimitHeaders reads the main limit's windows and credit state off
// response headers.
func parseCodexRateLimitHeaders(header http.Header, now time.Time) (cliproxyexecutor.QuotaObservation, bool) {
	observation := cliproxyexecutor.QuotaObservation{ObservedAt: now}
	for _, slot := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + slot + "-"
		var in codexRateLimitWindowInput
		in.usedPercent, in.hasUsed = codexHeaderFloat(header, prefix+"Used-Percent")
		minutes, hasMinutes := codexHeaderInt64(header, prefix+"Window-Minutes")
		in.windowMinutes, in.hasWindowMinute = minutes, hasMinutes
		in.resetAt, _ = codexHeaderInt64(header, prefix+"Reset-At")
		in.resetAfterSecs, in.hasResetAfter = codexHeaderInt64(header, prefix+"Reset-After-Seconds")
		if window, ok := newCodexRateLimitWindow(in, now); ok {
			observation.Windows = append(observation.Windows, window)
		}
	}
	if len(observation.Windows) == 0 {
		return cliproxyexecutor.QuotaObservation{}, false
	}
	hasCredits, okCredits := codexHeaderBool(header, "X-Codex-Credits-Has-Credits")
	unlimited, okUnlimited := codexHeaderBool(header, "X-Codex-Credits-Unlimited")
	observation.OverageAvailable = (okCredits && hasCredits) || (okUnlimited && unlimited)
	return observation, true
}

// parseCodexRateLimitEvent reads a `codex.rate_limits` stream event. It returns
// false for any other payload and for an event about a separately metered limit.
func parseCodexRateLimitEvent(payload []byte, now time.Time) (cliproxyexecutor.QuotaObservation, bool) {
	if !bytes.Contains(payload, codexRateLimitsEventMarker) || gjson.GetBytes(payload, "type").String() != codexRateLimitsEventType {
		return cliproxyexecutor.QuotaObservation{}, false
	}
	for _, field := range []string{"metered_limit_name", "limit_name"} {
		if name := strings.TrimSpace(gjson.GetBytes(payload, field).String()); name != "" && !isCodexMainRateLimitName(name) {
			return cliproxyexecutor.QuotaObservation{}, false
		}
	}
	observation := cliproxyexecutor.QuotaObservation{ObservedAt: now}
	limits := gjson.GetBytes(payload, "rate_limits")
	for _, slot := range []string{"primary", "secondary"} {
		raw := limits.Get(slot)
		if !raw.IsObject() {
			continue
		}
		var in codexRateLimitWindowInput
		if used := raw.Get("used_percent"); used.Type == gjson.Number {
			in.usedPercent, in.hasUsed = used.Float(), true
		}
		if minutes := raw.Get("window_minutes"); minutes.Type == gjson.Number {
			in.windowMinutes, in.hasWindowMinute = minutes.Int(), true
		}
		if resetAt := raw.Get("reset_at"); resetAt.Type == gjson.Number {
			in.resetAt = resetAt.Int()
		}
		if after := raw.Get("reset_after_seconds"); after.Type == gjson.Number {
			in.resetAfterSecs, in.hasResetAfter = after.Int(), true
		}
		if window, ok := newCodexRateLimitWindow(in, now); ok {
			observation.Windows = append(observation.Windows, window)
		}
	}
	if len(observation.Windows) == 0 {
		return cliproxyexecutor.QuotaObservation{}, false
	}
	credits := gjson.GetBytes(payload, "credits")
	observation.OverageAvailable = credits.Get("has_credits").Bool() || credits.Get("unlimited").Bool()
	return observation, true
}

// isCodexMainRateLimitName reports whether a limit name from an event is the
// account's main limit. The wire form varies in case and separator ("codex",
// "Codex", "codex_..."), so only the exact main id counts.
func isCodexMainRateLimitName(name string) bool {
	name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	return name == codexMainRateLimitName
}

// codexHeaderBool reads true/false/1/0 the way the official client does.
func codexHeaderBool(header http.Header, key string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(header.Get(key))) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

// observeCodexRateLimits reports the windows on a finished HTTP exchange: the
// response headers now, and any `codex.rate_limits` event as the body is read.
// It only acts for ChatGPT accounts on the ChatGPT backend and only when the
// scheduler is listening.
func observeCodexRateLimits(ctx context.Context, auth *cliproxyauth.Auth, target *url.URL, resp *http.Response) {
	if resp == nil || !cliproxyexecutor.HasQuotaObserver(ctx) || !isCodexChatGPTBackend(auth, target) {
		return
	}
	if observation, ok := parseCodexRateLimitHeaders(resp.Header, time.Now()); ok {
		cliproxyexecutor.ObserveQuota(ctx, observation)
	}
	if resp.Body != nil && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		resp.Body = &codexRateLimitBody{ReadCloser: resp.Body, ctx: ctx}
	}
}

// observeCodexWebsocketHandshake reports the windows on a websocket upgrade
// answer, which carries the same headers as an HTTP answer.
func observeCodexWebsocketHandshake(ctx context.Context, chatGPTBackend bool, resp *http.Response) {
	if resp == nil || !chatGPTBackend || !cliproxyexecutor.HasQuotaObserver(ctx) {
		return
	}
	if observation, ok := parseCodexRateLimitHeaders(resp.Header, time.Now()); ok {
		cliproxyexecutor.ObserveQuota(ctx, observation)
	}
}

// observeCodexWebsocketMessage reports a `codex.rate_limits` event received on a
// websocket connection; every other message is ignored.
func observeCodexWebsocketMessage(ctx context.Context, chatGPTBackend bool, payload []byte) {
	if !chatGPTBackend || !bytes.Contains(payload, codexRateLimitsEventMarker) || !cliproxyexecutor.HasQuotaObserver(ctx) {
		return
	}
	if observation, ok := parseCodexRateLimitEvent(payload, time.Now()); ok {
		cliproxyexecutor.ObserveQuota(ctx, observation)
	}
}

// codexRateLimitBody passes a response body through byte for byte while looking
// for `codex.rate_limits` events in the server-sent stream it carries.
type codexRateLimitBody struct {
	io.ReadCloser
	ctx context.Context
	// line holds the start of the line being read, up to the event size bound.
	line []byte
	// skipLine is set once the current line outgrows that bound: it is content,
	// not an event, and is not worth keeping.
	skipLine bool
}

func (b *codexRateLimitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.scan(p[:n])
	}
	if err == io.EOF {
		b.endLine()
	}
	return n, err
}

func (b *codexRateLimitBody) scan(chunk []byte) {
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		if newline < 0 {
			b.keep(chunk)
			return
		}
		b.keep(chunk[:newline])
		b.endLine()
		chunk = chunk[newline+1:]
	}
}

func (b *codexRateLimitBody) keep(part []byte) {
	if b.skipLine {
		return
	}
	if len(b.line)+len(part) > codexRateLimitMaxEventLine {
		b.skipLine = true
		b.line = b.line[:0]
		return
	}
	b.line = append(b.line, part...)
}

func (b *codexRateLimitBody) endLine() {
	line, skipped := bytes.TrimSpace(b.line), b.skipLine
	b.line, b.skipLine = b.line[:0], false
	if skipped || !bytes.HasPrefix(line, dataTag) {
		return
	}
	observeCodexEventPayload(b.ctx, bytes.TrimSpace(line[len(dataTag):]))
}

func observeCodexEventPayload(ctx context.Context, payload []byte) {
	if !bytes.Contains(payload, codexRateLimitsEventMarker) {
		return
	}
	if observation, ok := parseCodexRateLimitEvent(payload, time.Now()); ok {
		cliproxyexecutor.ObserveQuota(ctx, observation)
	}
}
