package config

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// providerStableIDNamespace scopes the name-based UUIDs derived below.
var providerStableIDNamespace = uuid.MustParse("5b6f2f64-3c8e-4f0a-9c5d-1f3b8e7a2d41")

// EnsureProviderStableIDsDeterministic assigns IDs like EnsureProviderStableIDs
// but derives the missing ones from seed (the tenant), the channel kind, the
// position and the entry content instead of drawing random UUIDs.
//
// This is for read paths that fill in IDs for rows stored without them. Several
// nodes load the same row concurrently; with random IDs each node invented its
// own set and wrote it back, so the nodes disagreed on the IDs that bindings
// point at and the last writer won. Derived IDs are the same on every node, so
// the write-back is idempotent. Valid, unique IDs are never changed.
func (cfg *Config) EnsureProviderStableIDsDeterministic(seed string) bool {
	if cfg == nil {
		return false
	}
	d := &stableIDDeriver{seed: strings.TrimSpace(seed), seen: make(map[string]struct{})}
	ensureStableIDs(d, "gemini", cfg.GeminiKey, func(v *GeminiKey) *string { return &v.ID })
	ensureStableIDs(d, "codex", cfg.CodexKey, func(v *CodexKey) *string { return &v.ID })
	ensureStableIDs(d, "claude", cfg.ClaudeKey, func(v *ClaudeKey) *string { return &v.ID })
	ensureStableIDs(d, "bedrock", cfg.BedrockKey, func(v *BedrockKey) *string { return &v.ID })
	ensureStableIDs(d, "opencode-go", cfg.OpenCodeGoKey, func(v *OpenCodeGoKey) *string { return &v.ID })
	ensureStableIDs(d, "cline", cfg.ClineKey, func(v *ClineKey) *string { return &v.ID })
	ensureStableIDs(d, "ollama-cloud", cfg.OllamaCloudKey, func(v *OllamaCloudKey) *string { return &v.ID })
	ensureStableIDs(d, "commandcode", cfg.CommandCodeKey, func(v *CommandCodeKey) *string { return &v.ID })
	for i := range cfg.OpenAICompatibility {
		ensureStableIDs(d, "openai-compatibility", cfg.OpenAICompatibility[i:i+1], func(v *OpenAICompatibility) *string { return &v.ID }, i)
		ensureStableIDs(d, "openai-compatibility/"+strconv.Itoa(i)+"/api-key-entries", cfg.OpenAICompatibility[i].APIKeyEntries,
			func(v *OpenAICompatibilityAPIKey) *string { return &v.ID })
	}
	ensureStableIDs(d, "vertex", cfg.VertexCompatAPIKey, func(v *VertexCompatKey) *string { return &v.ID })
	return d.changed
}

type stableIDDeriver struct {
	seed    string
	seen    map[string]struct{}
	changed bool
}

// ensureStableIDs normalises or derives the ID of every entry in list. offset,
// when given, is the position of list[0] in its parent list (the compat entries
// are visited one at a time so their nested keys follow them in order, which is
// the order EnsureProviderStableIDs uses too).
func ensureStableIDs[T any](d *stableIDDeriver, kind string, list []T, id func(*T) *string, offset ...int) {
	base := 0
	if len(offset) > 0 {
		base = offset[0]
	}
	for i := range list {
		ptr := id(&list[i])
		trimmed := strings.TrimSpace(*ptr)
		if parsed, err := uuid.Parse(trimmed); err == nil {
			normalized := parsed.String()
			if _, dup := d.seen[normalized]; !dup {
				d.seen[normalized] = struct{}{}
				if *ptr != normalized {
					*ptr = normalized
					d.changed = true
				}
				continue
			}
		}
		// Fingerprint the entry without its (missing or clashing) ID.
		original := *ptr
		*ptr = ""
		fingerprint, _ := json.Marshal(list[i])
		*ptr = original
		name := d.seed + "\x00" + kind + "\x00" + strconv.Itoa(base+i) + "\x00" + string(fingerprint)
		derived := uuid.NewSHA1(providerStableIDNamespace, []byte(name)).String()
		for attempt := 1; ; attempt++ {
			if _, dup := d.seen[derived]; !dup {
				break
			}
			derived = uuid.NewSHA1(providerStableIDNamespace, []byte(name+"\x00"+strconv.Itoa(attempt))).String()
		}
		d.seen[derived] = struct{}{}
		*ptr = derived
		d.changed = true
	}
}
