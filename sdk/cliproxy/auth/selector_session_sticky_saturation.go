package auth

import (
	"context"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Session stickiness and per-account concurrency limits.
//
// Providers scope the prompt cache per upstream account, so a sticky binding is
// what keeps a conversation on its cache; moving it makes the next turn pay
// full price for the whole history, ten times a cached turn on Codex. An
// account at its concurrency limit is busy for seconds, not unusable, yet
// selection used to drop it from the candidates before the sticky selector saw
// them: that read as "the bound account is gone", released the binding and
// moved the conversation for good, and two accounts taking turns at their limit
// moved running conversations back and forth. (2026-11-20: one tenant's two
// accounts, limit 5 each, did that for under a minute; queuing would have
// waited 1.5s at the median and 4.2s at most.)
//
// So a binding stands while its account is saturated. The sticky selector sees
// saturated accounts, honours the binding and names the account in the
// request's stickyPickNote; nextReadyCandidate queues on that account instead
// of failing over. A request whose wait runs out moves alone: the note's
// overflow flag makes its later picks serve it elsewhere without releasing or
// rewriting the binding, so the conversation's next turn goes back. New
// bindings still go to accounts with room.

// stickyPickNote carries what session stickiness decided for one request across
// its picks. The execution service keeps one per request and hands it to
// selection through the context.
type stickyPickNote struct {
	// bound is the account the latest pick kept or started a session binding
	// on, "" when that pick was not sticky.
	bound string
	// overflow is set once the bound account could not take the request within
	// the queue wait: the request's later picks serve it elsewhere and leave the
	// binding alone.
	overflow bool
}

type stickyPickNoteKey struct{}

func withStickyPickNote(ctx context.Context, note *stickyPickNote) context.Context {
	if note == nil {
		return ctx
	}
	return context.WithValue(ctx, stickyPickNoteKey{}, note)
}

func stickyPickNoteFrom(ctx context.Context) *stickyPickNote {
	if ctx == nil {
		return nil
	}
	note, _ := ctx.Value(stickyPickNoteKey{}).(*stickyPickNote)
	return note
}

func (n *stickyPickNote) record(authID string) {
	if n != nil {
		n.bound = authID
	}
}

// holds reports whether authID is the account the request's session is bound
// to, with the request still willing to wait for it.
func (n *stickyPickNote) holds(authID string) bool {
	return n != nil && !n.overflow && n.bound != "" && n.bound == authID
}

// pick is Pick with the candidates split by concurrency: eligible holds every
// candidate, saturated or not, and unsaturated those with a free slot (all of
// them when none has one, as AccountConcurrencyLimiter.FilterAvailableCandidates
// returns them).
func (s *SessionStickySelector) pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, eligible, unsaturated []*Auth) (*Auth, error) {
	note := stickyPickNoteFrom(ctx)
	key := sessionStickySelectionKey(provider, opts, sessionStickyKey(opts))
	if key == "" || (note != nil && note.overflow) {
		// No conversation to keep, or one whose account could not take this
		// request in time: serve it where there is room, binding untouched.
		return s.pickFallback(ctx, provider, model, opts, unsaturated)
	}

	now := time.Now()
	available, err := getAvailableAuths(eligible, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	limits := stickyLimitsFromMetadata(opts.Metadata)
	if bound := s.honour(ctx, key, model, available, limits, now); bound != nil {
		note.record(bound.ID)
		return bound, nil
	}

	selected, err := s.pickFallback(ctx, provider, model, opts, authsWithRoom(available, unsaturated))
	if err != nil {
		return nil, err
	}
	if selected != nil && strings.TrimSpace(selected.ID) != "" {
		// The full list, not the narrowed one: a binding another process made
		// first wins even while its account is saturated.
		selected = s.bindSelected(ctx, key, selected, available, now)
		note.record(selected.ID)
	}
	return selected, nil
}

// authsWithRoom keeps the accounts of available that are also in unsaturated,
// in available's order. When none is (every account the request may use is at
// its limit) it returns available, so the request queues instead of failing.
func authsWithRoom(available, unsaturated []*Auth) []*Auth {
	if len(available) == 0 || len(unsaturated) == 0 {
		return available
	}
	room := make(map[string]struct{}, len(unsaturated))
	for _, auth := range unsaturated {
		if auth != nil {
			room[auth.ID] = struct{}{}
		}
	}
	out := make([]*Auth, 0, len(available))
	for _, auth := range available {
		if auth == nil {
			continue
		}
		if _, ok := room[auth.ID]; ok {
			out = append(out, auth)
		}
	}
	if len(out) == 0 {
		return available
	}
	return out
}
