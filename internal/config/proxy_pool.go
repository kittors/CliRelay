package config

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// ProxyPoolEntry describes a reusable outbound proxy managed by operators.
type ProxyPoolEntry struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	URL         string `yaml:"url" json:"url"`
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// ValidateProxyURL verifies that a proxy URL can be used by the shared transport builders.
func ValidateProxyURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("proxy url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid proxy url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("proxy url must include scheme and host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5":
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", parsed.Scheme)
	}
}

// NormalizeProxyPool trims entries, removes invalid rows and keeps the first entry per ID.
func NormalizeProxyPool(entries []ProxyPoolEntry) []ProxyPoolEntry {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]ProxyPoolEntry, 0, len(entries))
	for _, entry := range entries {
		entry = normalizeProxyPoolEntry(entry)
		if entry.URL == "" {
			continue
		}
		if _, ok := seen[entry.ID]; ok {
			continue
		}
		seen[entry.ID] = struct{}{}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ProxyPoolDuplicateIDs returns normalized ids that appear more than once among valid entries.
// Used by the management API so silent first-wins drops cannot hide a failed create.
func ProxyPoolDuplicateIDs(entries []ProxyPoolEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	var dups []string
	dupSeen := make(map[string]struct{})
	for _, entry := range entries {
		entry = normalizeProxyPoolEntry(entry)
		if entry.URL == "" {
			continue
		}
		if _, ok := seen[entry.ID]; ok {
			if _, reported := dupSeen[entry.ID]; !reported {
				dups = append(dups, entry.ID)
				dupSeen[entry.ID] = struct{}{}
			}
			continue
		}
		seen[entry.ID] = struct{}{}
	}
	return dups
}

func normalizeProxyPoolEntry(entry ProxyPoolEntry) ProxyPoolEntry {
	entry.ID = normalizeProxyID(entry.ID)
	entry.Name = strings.TrimSpace(entry.Name)
	entry.URL = strings.TrimSpace(entry.URL)
	entry.Description = strings.TrimSpace(entry.Description)
	if entry.URL == "" || ValidateProxyURL(entry.URL) != nil {
		entry.URL = ""
		return entry
	}
	if entry.ID == "" {
		// Prefer a stable id from the full proxy URL (host+auth+port) so
		// Chinese-only names do not all collapse to the same slug.
		entry.ID = proxyIDFromURL(entry.URL)
	}
	if entry.Name == "" {
		entry.Name = entry.ID
	}
	return entry
}

// SanitizeProxyPool normalizes the configured reusable proxy list in-place.
func (cfg *Config) SanitizeProxyPool() {
	if cfg == nil {
		return
	}
	cfg.ProxyPool = NormalizeProxyPool(cfg.ProxyPool)
}

// ResolveProxyURL returns the effective proxy URL for a proxy-id plus legacy fallback URL.
func (cfg *Config) ResolveProxyURL(proxyID string, fallbackURL string) string {
	if cfg != nil {
		id := normalizeProxyID(proxyID)
		if id != "" {
			for _, entry := range cfg.ProxyPool {
				if entry.Enabled && normalizeProxyID(entry.ID) == id && strings.TrimSpace(entry.URL) != "" {
					return strings.TrimSpace(entry.URL)
				}
			}
		}
	}
	if fallback := strings.TrimSpace(fallbackURL); fallback != "" {
		return fallback
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

// FindProxyPoolEntry finds a proxy pool entry by ID regardless of whether it is enabled.
func (cfg *Config) FindProxyPoolEntry(proxyID string) *ProxyPoolEntry {
	if cfg == nil {
		return nil
	}
	id := normalizeProxyID(proxyID)
	if id == "" {
		return nil
	}
	for i := range cfg.ProxyPool {
		if normalizeProxyID(cfg.ProxyPool[i].ID) == id {
			return &cfg.ProxyPool[i]
		}
	}
	return nil
}

// NormalizeProxyID exposes the shared proxy ID normalization used by storage,
// management handlers, and runtime resolution.
func NormalizeProxyID(raw string) string {
	return normalizeProxyID(raw)
}

func normalizeProxyID(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func proxyIDFromURL(raw string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(raw)))
	return "proxy-" + hex.EncodeToString(sum[:])[:10]
}
