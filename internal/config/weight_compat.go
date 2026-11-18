package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
)

const MaxCredentialWeight = int(credentialweight.Max)

func ValidateCredentialWeight(weight *int) error {
	if weight == nil {
		return nil
	}
	_, errNormalize := credentialweight.Normalize(int64(*weight))
	return errNormalize
}

func (cfg *Config) ValidateCredentialWeights() error {
	if cfg == nil {
		return nil
	}
	validate := func(path string, weight *int) error {
		if err := ValidateCredentialWeight(weight); err != nil {
			return fmt.Errorf("%s.weight: %w", path, err)
		}
		return nil
	}
	for i := range cfg.GeminiKey {
		if err := validate(fmt.Sprintf("gemini-api-key[%d]", i), cfg.GeminiKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.InteractionsKey {
		if err := validate(fmt.Sprintf("interactions-api-key[%d]", i), cfg.InteractionsKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.ClaudeKey {
		if err := validate(fmt.Sprintf("claude-api-key[%d]", i), cfg.ClaudeKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.VertexCompatAPIKey {
		if err := validate(fmt.Sprintf("vertex-api-key[%d]", i), cfg.VertexCompatAPIKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.CodexKey {
		if err := validate(fmt.Sprintf("codex-api-key[%d]", i), cfg.CodexKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.XAIKey {
		if err := validate(fmt.Sprintf("xai-api-key[%d]", i), cfg.XAIKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.MetaKey {
		if err := validate(fmt.Sprintf("meta-api-key[%d]", i), cfg.MetaKey[i].Weight); err != nil {
			return err
		}
	}
	for i := range cfg.OpenAICompatibility {
		if err := validate(fmt.Sprintf("openai-compatibility[%d]", i), cfg.OpenAICompatibility[i].Weight); err != nil {
			return err
		}
		for j := range cfg.OpenAICompatibility[i].APIKeyEntries {
			if err := validate(fmt.Sprintf("openai-compatibility[%d].api-key-entries[%d]", i, j), cfg.OpenAICompatibility[i].APIKeyEntries[j].Weight); err != nil {
				return err
			}
		}
	}
	return nil
}

func FormatConnectionPoolDedupID(pool *ProxyConnectionPoolConfig) string {
	if pool == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("pool:")
	if pool.Enabled != nil {
		b.WriteString(strconv.FormatBool(*pool.Enabled))
	}
	b.WriteByte(0)
	b.WriteString(strings.TrimSpace(pool.IdleConnTimeout))
	b.WriteByte(0)
	if pool.MaxIdleConnsPerHost != nil {
		b.WriteString(strconv.Itoa(*pool.MaxIdleConnsPerHost))
	}
	return b.String()
}

func FormatSortedHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte(0)
		b.WriteString(headers[key])
		b.WriteByte(0)
	}
	return b.String()
}
