package config

import (
	"sort"
	"strings"
)

// NormalizeSensitiveWordsReplace trims replacement target keys, drops blank keys,
// and preserves replacement values, including empty strings for phrase deletion.
func NormalizeSensitiveWordsReplace(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(in))
	for _, rawKey := range keys {
		trimmedKey := strings.TrimSpace(rawKey)
		if trimmedKey == "" {
			continue
		}
		if _, exists := out[trimmedKey]; exists && rawKey != trimmedKey {
			continue
		}
		out[trimmedKey] = in[rawKey]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
