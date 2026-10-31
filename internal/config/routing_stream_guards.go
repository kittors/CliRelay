package config

import (
	"fmt"
	"time"
)

// A channel group's stream guards bound two waits that would otherwise last as
// long as the client is willing to hold the connection open.
//
// first-token-timeout-seconds (0 or unset: off, the default) gives up on an
// upstream that has not produced its first non-empty chunk within that many
// seconds of the request being sent, so a stalled account can be swapped for
// another one before the client has seen any byte. It only applies to
// streaming requests.
//
// busy-wait-max-seconds (0 or unset: off, the default) lets a request wait,
// instead of failing at once, when every account it may use is cooling down
// after a recoverable failure (408, a 5xx, a short 429): it waits for the
// soonest one to recover, for at most that many seconds in total and in at
// most two waits (sdk/cliproxy/auth/busy_wait.go). Brief load-shed back-offs of
// a few seconds are not waited for. Turn it on for groups whose clients prefer
// a late answer to an immediate error (60 is the usual choice: it outlasts the
// one-minute back-off a 5xx or a short 429 sets by default), and weigh two
// things first:
//   - A streaming client is kept informed with keep-alive comments once the wait
//     passes the keep-alive interval, which commits the response as 200; a
//     failure after that reaches the client as an error event inside the
//     stream, not as an HTTP status.
//   - A non-streaming request, an image or a video submission waits too and the
//     client hears nothing until the wait ends, so a client whose own timeout is
//     shorter than the wait counts it as a failure.
//
// Requests confined to one group use that group's values; every other request
// (no route group, or several allowed groups) uses the values of the group
// named "default".
const (
	// MinFirstTokenTimeoutSeconds keeps the timeout clear of normal start-up
	// latency: below 5 s a healthy upstream that is queueing the request, or a
	// proxy still completing its TLS handshake, would be abandoned.
	MinFirstTokenTimeoutSeconds = 5
	// MaxFirstTokenTimeoutSeconds: past five minutes the client's own timeout
	// is the better judge of a stalled request.
	MaxFirstTokenTimeoutSeconds = 300
	// MaxBusyWaitMaxSeconds keeps the worst case well inside the 10-minute
	// request timeout of the official Anthropic and OpenAI SDKs, leaving the
	// rest of that budget for the answer itself.
	MaxBusyWaitMaxSeconds = 240
)

// FirstTokenTimeout returns the group's first-token timeout, 0 when it is off.
func (g RoutingChannelGroup) FirstTokenTimeout() time.Duration {
	seconds := clampFirstTokenTimeoutSeconds(g.FirstTokenTimeoutSeconds)
	return time.Duration(seconds) * time.Second
}

// BusyWaitMax returns how long a request of this group may wait for a cooling
// account, 0 when waiting is off. Waiting is opt-in: unset is off, like 0. The
// stored form still keeps the two apart (an explicit 0 survives a save) so the
// operator's choice stays visible and survives a future change of the default.
func (g RoutingChannelGroup) BusyWaitMax() time.Duration {
	if g.BusyWaitMaxSeconds == nil {
		return 0
	}
	return time.Duration(clampBusyWaitMaxSeconds(*g.BusyWaitMaxSeconds)) * time.Second
}

// ValidateStreamGuards reports a value outside the accepted range. The
// management API rejects those instead of letting SanitizeRouting quietly
// change what the operator typed.
func (g RoutingChannelGroup) ValidateStreamGuards() error {
	if seconds := g.FirstTokenTimeoutSeconds; seconds != 0 && (seconds < MinFirstTokenTimeoutSeconds || seconds > MaxFirstTokenTimeoutSeconds) {
		return fmt.Errorf("channel group %q: first-token-timeout-seconds must be 0 (off) or between %d and %d, got %d",
			g.Name, MinFirstTokenTimeoutSeconds, MaxFirstTokenTimeoutSeconds, seconds)
	}
	if g.BusyWaitMaxSeconds != nil {
		if seconds := *g.BusyWaitMaxSeconds; seconds < 0 || seconds > MaxBusyWaitMaxSeconds {
			return fmt.Errorf("channel group %q: busy-wait-max-seconds must be between 0 (off) and %d, got %d",
				g.Name, MaxBusyWaitMaxSeconds, seconds)
		}
	}
	return nil
}

// ValidateRoutingStreamGuards checks every group of a routing config.
func ValidateRoutingStreamGuards(routing RoutingConfig) error {
	for _, group := range routing.ChannelGroups {
		if err := group.ValidateStreamGuards(); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeStreamGuards brings stored values into range, for configs that did
// not come through the management API (an import, a hand-edited row, an older
// panel). Out-of-range values move to the nearest safe one: a negative timeout
// or wait is read as "off", never as "wait forever".
func sanitizeStreamGuards(group *RoutingChannelGroup) {
	if group == nil {
		return
	}
	group.FirstTokenTimeoutSeconds = clampFirstTokenTimeoutSeconds(group.FirstTokenTimeoutSeconds)
	if group.BusyWaitMaxSeconds != nil {
		seconds := clampBusyWaitMaxSeconds(*group.BusyWaitMaxSeconds)
		group.BusyWaitMaxSeconds = &seconds
	}
}

func clampFirstTokenTimeoutSeconds(seconds int) int {
	switch {
	case seconds <= 0:
		return 0
	case seconds < MinFirstTokenTimeoutSeconds:
		return MinFirstTokenTimeoutSeconds
	case seconds > MaxFirstTokenTimeoutSeconds:
		return MaxFirstTokenTimeoutSeconds
	default:
		return seconds
	}
}

func clampBusyWaitMaxSeconds(seconds int) int {
	switch {
	case seconds <= 0:
		return 0
	case seconds > MaxBusyWaitMaxSeconds:
		return MaxBusyWaitMaxSeconds
	default:
		return seconds
	}
}
