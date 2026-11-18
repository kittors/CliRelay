package config

import (
	"math"
	"time"
)

// ClaudeConfig configures provider-wide Claude request behavior.
type ClaudeConfig struct {
	ModelLevelCooling  bool `yaml:"model-level-cooling" json:"model-level-cooling"`
	ToolSearch         bool `yaml:"tool-search" json:"tool-search"`
	ServerSideFallback bool `yaml:"server-side-fallback" json:"server-side-fallback"`
	SessionDatePinDays *int `yaml:"session-date-pin-days" json:"session-date-pin-days"`
}

const DefaultClaudeSessionDatePinDays = 7

func (c ClaudeConfig) SessionDatePinRetention() time.Duration {
	days := DefaultClaudeSessionDatePinDays
	if c.SessionDatePinDays != nil {
		days = *c.SessionDatePinDays
	}
	if days <= 0 {
		return 0
	}
	maxDays := int(time.Duration(math.MaxInt64) / (24 * time.Hour))
	if days > maxDays {
		days = maxDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// XAIConfig configures provider-wide xAI request behavior.
type XAIConfig struct {
	InjectXSearch bool `yaml:"inject-x-search" json:"inject-x-search"`
}

// TLSHandshakeTimeout returns the configured upstream TLS handshake bound.
func (c *SDKConfig) TLSHandshakeTimeout() time.Duration {
	if c == nil || c.TLSHandshakeTimeoutSeconds <= 0 {
		return 0
	}
	maxSeconds := int64(math.MaxInt64 / int64(time.Second))
	if int64(c.TLSHandshakeTimeoutSeconds) > maxSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(c.TLSHandshakeTimeoutSeconds) * time.Second
}
