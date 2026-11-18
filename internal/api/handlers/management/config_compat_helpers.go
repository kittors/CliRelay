package management

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func normalizeOpenAICompatibilityEntry(entry *config.OpenAICompatibility) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), entry.APIKeyEntries...)
	for i := range entry.APIKeyEntries {
		entry.APIKeyEntries[i].APIKey = strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
	}
	entry.Headers = config.NormalizeHeaders(entry.Headers)
}

func normalizedOpenAICompatibilityEntries(entries []config.OpenAICompatibility) []config.OpenAICompatibility {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenAICompatibility, len(entries))
	for i := range entries {
		out[i] = entries[i]
		normalizeOpenAICompatibilityEntry(&out[i])
	}
	return out
}
