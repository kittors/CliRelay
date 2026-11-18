package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"time"
)

// Modes of codex-upstream-incremental.
const (
	// CodexUpstreamIncrementalOff leaves every Codex request on its usual
	// transport and keeps no per-conversation state.
	CodexUpstreamIncrementalOff = "off"
	// CodexUpstreamIncrementalShadow compares each request with the previous turn
	// of its conversation and records whether it could have gone out as an
	// increment, without changing what is sent.
	CodexUpstreamIncrementalShadow = "shadow"
	// CodexUpstreamIncrementalOn sends the requests of allowed API keys over a
	// websocket pinned to their conversation, carrying only what each turn adds;
	// every other request is compared as in shadow mode.
	CodexUpstreamIncrementalOn = "on"
)

const (
	// DefaultCodexUpstreamIncrementalMaxFirstRequestMB bounds the full request
	// that opens a chain over the websocket. Larger first requests keep going out
	// over HTTP. 15 MB follows sub2api, the only other relay with this transport
	// in production; the backend's own frame limit has not been measured.
	DefaultCodexUpstreamIncrementalMaxFirstRequestMB = 15
	// DefaultCodexUpstreamIncrementalMaxChains bounds the conversations that
	// keep an upstream websocket open between turns.
	DefaultCodexUpstreamIncrementalMaxChains = 256
	// DefaultCodexUpstreamIncrementalMaxChainsPerAuth bounds the open upstream
	// websockets one account carries.
	DefaultCodexUpstreamIncrementalMaxChainsPerAuth = 16
	// DefaultCodexUpstreamIncrementalIdleTimeout frees a conversation's upstream
	// websocket after this long without a turn. 98% of the gaps between turns
	// measured on 2026-11-18 were under 30 seconds.
	DefaultCodexUpstreamIncrementalIdleTimeout = 5 * time.Minute
)

// CodexUpstreamIncrementalConfig controls incremental requests to the Codex
// upstream (codex-upstream-incremental).
//
// Over HTTP a Codex request carries the whole conversation every turn, and with
// store=false so does the request this relay sends on: screenshots included,
// the same bytes leave the relay's egress every turn (94% of them repeats on
// 2026-11-18). On one upstream websocket the backend keeps the last response in
// memory, so a turn can send only its new input plus previous_response_id. The
// relay does that comparison itself, so clients that resend everything (HTTP
// Responses, Chat Completions) benefit without changing anything.
type CodexUpstreamIncrementalConfig struct {
	// Mode is off (the default), shadow or on; see the constants above.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// AllowKeys limits "on" to these downstream API keys; requests of any other
	// key are only compared, as in shadow mode. Empty means every key. Entries
	// are the key itself or "sha256:" followed by the hex SHA-256 of the key,
	// which keeps the key out of the config file.
	AllowKeys []string `yaml:"allow-keys,omitempty" json:"allow-keys,omitempty"`
	// MaxFirstRequestMB caps the full request that starts a chain over the
	// websocket; 0 means the default (15).
	MaxFirstRequestMB int `yaml:"max-first-request-mb,omitempty" json:"max-first-request-mb,omitempty"`
	// MaxChains caps the conversations holding an upstream websocket; 0 means
	// the default (256).
	MaxChains int `yaml:"max-chains,omitempty" json:"max-chains,omitempty"`
	// MaxChainsPerAuth caps the upstream websockets one account holds; 0 means
	// the default (16).
	MaxChainsPerAuth int `yaml:"max-chains-per-auth,omitempty" json:"max-chains-per-auth,omitempty"`
	// IdleTimeoutSeconds closes a conversation's upstream websocket after this
	// long without a turn; 0 means the default (300).
	IdleTimeoutSeconds int `yaml:"idle-timeout-seconds,omitempty" json:"idle-timeout-seconds,omitempty"`
}

// CodexUpstreamIncrementalMode returns the configured mode, off for anything it
// does not recognise.
func (c *Config) CodexUpstreamIncrementalMode() string {
	if c == nil {
		return CodexUpstreamIncrementalOff
	}
	switch mode := strings.ToLower(strings.TrimSpace(c.CodexUpstreamIncremental.Mode)); mode {
	case CodexUpstreamIncrementalShadow, CodexUpstreamIncrementalOn:
		return mode
	default:
		return CodexUpstreamIncrementalOff
	}
}

// CodexUpstreamIncrementalAllowsKey reports whether requests of this downstream
// API key may use the incremental websocket transport in "on" mode.
func (c *Config) CodexUpstreamIncrementalAllowsKey(apiKey string) bool {
	if c == nil {
		return false
	}
	entries := c.CodexUpstreamIncremental.AllowKeys
	if len(entries) == 0 {
		return true
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false
	}
	var digest string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if hexDigest, ok := strings.CutPrefix(entry, "sha256:"); ok {
			if digest == "" {
				sum := sha256.Sum256([]byte(apiKey))
				digest = hex.EncodeToString(sum[:])
			}
			if subtle.ConstantTimeCompare([]byte(strings.ToLower(strings.TrimSpace(hexDigest))), []byte(digest)) == 1 {
				return true
			}
			continue
		}
		if subtle.ConstantTimeCompare([]byte(entry), []byte(apiKey)) == 1 {
			return true
		}
	}
	return false
}

// CodexUpstreamIncrementalMaxFirstRequestBytes returns the largest full request
// that may open a chain over the websocket.
func (c *Config) CodexUpstreamIncrementalMaxFirstRequestBytes() int {
	mb := DefaultCodexUpstreamIncrementalMaxFirstRequestMB
	if c != nil && c.CodexUpstreamIncremental.MaxFirstRequestMB > 0 {
		mb = c.CodexUpstreamIncremental.MaxFirstRequestMB
	}
	return mb << 20
}

// CodexUpstreamIncrementalMaxChains returns how many conversations may hold an
// upstream websocket at once.
func (c *Config) CodexUpstreamIncrementalMaxChains() int {
	if c != nil && c.CodexUpstreamIncremental.MaxChains > 0 {
		return c.CodexUpstreamIncremental.MaxChains
	}
	return DefaultCodexUpstreamIncrementalMaxChains
}

// CodexUpstreamIncrementalMaxChainsPerAuth returns how many upstream websockets
// one account may hold.
func (c *Config) CodexUpstreamIncrementalMaxChainsPerAuth() int {
	if c != nil && c.CodexUpstreamIncremental.MaxChainsPerAuth > 0 {
		return c.CodexUpstreamIncremental.MaxChainsPerAuth
	}
	return DefaultCodexUpstreamIncrementalMaxChainsPerAuth
}

// CodexUpstreamIncrementalIdleTimeout returns how long a conversation keeps its
// upstream websocket between turns.
func (c *Config) CodexUpstreamIncrementalIdleTimeout() time.Duration {
	if c != nil && c.CodexUpstreamIncremental.IdleTimeoutSeconds > 0 {
		return time.Duration(c.CodexUpstreamIncremental.IdleTimeoutSeconds) * time.Second
	}
	return DefaultCodexUpstreamIncrementalIdleTimeout
}
