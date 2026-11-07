package config

// CodexRequestZstdEnabled reports whether request bodies for the ChatGPT Codex
// backend's POST /responses are sent zstd-compressed.
//
// It defaults to on. Compression is what the official Codex client does for
// ChatGPT accounts, so enabling it moves this proxy's wire behaviour toward the
// client it presents itself as rather than away from it, and it is what keeps
// large histories from being rejected by the backend (a 1.1 MB plain body drew a
// 503 where the compressed one was accepted). That makes it a convergence
// feature, which defaults on, unlike impersonation features that depend on
// per-upstream measurement. The switch exists for operators who need to see the
// uncompressed bytes on the wire, for example behind an inspecting proxy.
func (c *Config) CodexRequestZstdEnabled() bool {
	return c == nil || c.CodexRequestZstd == nil || *c.CodexRequestZstd
}
