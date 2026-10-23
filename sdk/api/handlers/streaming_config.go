package handlers

import "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}
