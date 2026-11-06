package config

import "time"

const (
	// DefaultResponsesWebsocketMaxMessageMB matches the decoded body limit a model
	// POST gets by default, so a turn too large for one is too large for the other.
	DefaultResponsesWebsocketMaxMessageMB = DefaultModelRequestBodyMB
	// DefaultResponsesWebsocketMaxTranscriptMB bounds the transcript one connection
	// keeps between turns to expand the next increment.
	DefaultResponsesWebsocketMaxTranscriptMB = 64
	// DefaultResponsesWebsocketIdleTimeout frees a connection that waits this long
	// for its next turn.
	DefaultResponsesWebsocketIdleTimeout = 10 * time.Minute
)

// ResponsesWebsocketConfig tunes the client-facing Responses WebSocket entry
// (GET /v1/responses and /backend-api/codex/responses).
//
// Over this entry Codex sends only what a turn adds (previous_response_id plus
// the new input items); over HTTP it re-sends the whole conversation every turn,
// screenshots included, which is where most of the relay's inbound traffic came
// from. The connection keeps the transcript it needs to expand those increments,
// so its size and lifetime are bounded here.
type ResponsesWebsocketConfig struct {
	// Disabled answers the upgrade with 426 Upgrade Required. Codex takes that as
	// "use HTTP" and switches at once without retrying, so this turns the entry
	// off without failing any request.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Compression negotiates permessage-deflate with clients that offer it.
	// Nil means on.
	Compression *bool `yaml:"compression,omitempty" json:"compression,omitempty"`
	// MaxMessageMB caps a single client message. 0 means the default (128).
	MaxMessageMB int `yaml:"max-message-mb,omitempty" json:"max-message-mb,omitempty"`
	// MaxTranscriptMB caps the transcript a connection keeps between turns. Past
	// it the connection is closed once the turn has been answered, and the
	// client's next request opens a new one carrying the full input, as it would
	// over HTTP. 0 means the default (64).
	MaxTranscriptMB int `yaml:"max-transcript-mb,omitempty" json:"max-transcript-mb,omitempty"`
	// IdleTimeoutSeconds closes a connection that waits this long for its next
	// turn, freeing the transcript it holds; the client reconnects on its next
	// request. 0 means the default (600); a negative value disables the timeout.
	IdleTimeoutSeconds int `yaml:"idle-timeout-seconds,omitempty" json:"idle-timeout-seconds,omitempty"`
}

// ResponsesWebsocketEnabled reports whether the Responses WebSocket entry accepts
// upgrades.
func (c *SDKConfig) ResponsesWebsocketEnabled() bool {
	return c == nil || !c.ResponsesWebsocket.Disabled
}

// ResponsesWebsocketCompressionEnabled reports whether permessage-deflate is
// offered to clients. It defaults to on: Codex asks for it on every connection.
func (c *SDKConfig) ResponsesWebsocketCompressionEnabled() bool {
	return c == nil || c.ResponsesWebsocket.Compression == nil || *c.ResponsesWebsocket.Compression
}

// ResponsesWebsocketMaxMessageBytes returns the size limit of one client message.
func (c *SDKConfig) ResponsesWebsocketMaxMessageBytes() int64 {
	mb := DefaultResponsesWebsocketMaxMessageMB
	if c != nil && c.ResponsesWebsocket.MaxMessageMB > 0 {
		mb = c.ResponsesWebsocket.MaxMessageMB
	}
	return int64(mb) << 20
}

// ResponsesWebsocketMaxTranscriptBytes returns how much transcript a connection
// may keep between turns.
func (c *SDKConfig) ResponsesWebsocketMaxTranscriptBytes() int64 {
	mb := DefaultResponsesWebsocketMaxTranscriptMB
	if c != nil && c.ResponsesWebsocket.MaxTranscriptMB > 0 {
		mb = c.ResponsesWebsocket.MaxTranscriptMB
	}
	return int64(mb) << 20
}

// ResponsesWebsocketIdleTimeout returns how long a connection may wait for its
// next turn; zero means it waits indefinitely.
func (c *SDKConfig) ResponsesWebsocketIdleTimeout() time.Duration {
	if c == nil || c.ResponsesWebsocket.IdleTimeoutSeconds == 0 {
		return DefaultResponsesWebsocketIdleTimeout
	}
	if c.ResponsesWebsocket.IdleTimeoutSeconds < 0 {
		return 0
	}
	return time.Duration(c.ResponsesWebsocket.IdleTimeoutSeconds) * time.Second
}
