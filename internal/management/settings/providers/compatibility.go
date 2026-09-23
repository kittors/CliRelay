package providers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type OpenAICompatibilityPatch struct {
	Name            *string                             `json:"name"`
	Disabled        *bool                               `json:"disabled"`
	Prefix          *string                             `json:"prefix"`
	BaseURL         *string                             `json:"base-url"`
	APIKeyEntries   *[]config.OpenAICompatibilityAPIKey `json:"api-key-entries"`
	Models          *[]config.OpenAICompatibilityModel  `json:"models"`
	Headers         *map[string]string                  `json:"headers"`
	CodexToolBridge *bool                               `json:"codex-tool-bridge"`
}

func (s *Service) OpenAICompatibility() []config.OpenAICompatibility {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizedOpenAICompatibilityEntries(s.cfg.OpenAICompatibility)
}

func (s *Service) ReplaceOpenAICompatibility(entries []config.OpenAICompatibility) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	filtered := make([]config.OpenAICompatibility, 0, len(entries))
	for i := range entries {
		NormalizeOpenAICompatibilityEntry(&entries[i])
		if strings.TrimSpace(entries[i].BaseURL) != "" {
			filtered = append(filtered, entries[i])
		}
	}
	prev := append([]config.OpenAICompatibility(nil), s.cfg.OpenAICompatibility...)
	next := &config.Config{OpenAICompatibility: filtered}
	prepareProviderStableIDs(&config.Config{OpenAICompatibility: prev}, next)
	s.cfg.OpenAICompatibility = next.OpenAICompatibility
	s.cfg.SanitizeOpenAICompatibility()
	if err := s.runValidator(); err != nil {
		s.cfg.OpenAICompatibility = prev
		return err
	}
	return nil
}

func (s *Service) PatchOpenAICompatibility(index *int, name *string, patch OpenAICompatibilityPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.OpenAICompatibility) {
		targetIndex = *index
	}
	if targetIndex == -1 && name != nil {
		match := strings.TrimSpace(*name)
		for i := range s.cfg.OpenAICompatibility {
			if s.cfg.OpenAICompatibility[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.OpenAICompatibility[targetIndex]
	if patch.Name != nil {
		entry.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Disabled != nil {
		entry.Disabled = *patch.Disabled
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
	}
	if patch.BaseURL != nil {
		trimmed := strings.TrimSpace(*patch.BaseURL)
		if trimmed == "" {
			s.cfg.OpenAICompatibility = append(s.cfg.OpenAICompatibility[:targetIndex], s.cfg.OpenAICompatibility[targetIndex+1:]...)
			s.cfg.SanitizeOpenAICompatibility()
			return nil
		}
		entry.BaseURL = trimmed
	}
	if patch.APIKeyEntries != nil {
		next := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{APIKeyEntries: append([]config.OpenAICompatibilityAPIKey(nil), (*patch.APIKeyEntries)...)}}}
		previous := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{APIKeyEntries: entry.APIKeyEntries}}}
		prepareProviderStableIDs(previous, next)
		entry.APIKeyEntries = next.OpenAICompatibility[0].APIKeyEntries
	}
	if patch.Models != nil {
		entry.Models = append([]config.OpenAICompatibilityModel(nil), (*patch.Models)...)
	}
	if patch.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*patch.Headers)
	}
	if patch.CodexToolBridge != nil {
		entry.CodexToolBridge = *patch.CodexToolBridge
	}
	NormalizeOpenAICompatibilityEntry(&entry)
	prev := append([]config.OpenAICompatibility(nil), s.cfg.OpenAICompatibility...)
	s.cfg.OpenAICompatibility[targetIndex] = entry
	s.cfg.SanitizeOpenAICompatibility()
	if err := s.runValidator(); err != nil {
		s.cfg.OpenAICompatibility = prev
		return err
	}
	return nil
}

func (s *Service) DeleteOpenAICompatibilityByName(name string) {
	if s == nil || s.cfg == nil {
		return
	}
	out := make([]config.OpenAICompatibility, 0, len(s.cfg.OpenAICompatibility))
	for _, entry := range s.cfg.OpenAICompatibility {
		if entry.Name != name {
			out = append(out, entry)
		}
	}
	s.cfg.OpenAICompatibility = out
	s.cfg.SanitizeOpenAICompatibility()
}

func (s *Service) DeleteOpenAICompatibilityByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.OpenAICompatibility) {
		return false
	}
	s.cfg.OpenAICompatibility = append(s.cfg.OpenAICompatibility[:index], s.cfg.OpenAICompatibility[index+1:]...)
	s.cfg.SanitizeOpenAICompatibility()
	return true
}

type VertexCompatPatch struct {
	APIKey   *string                     `json:"api-key"`
	Prefix   *string                     `json:"prefix"`
	BaseURL  *string                     `json:"base-url"`
	ProxyURL *string                     `json:"proxy-url"`
	ProxyID  *string                     `json:"proxy-id"`
	Headers  *map[string]string          `json:"headers"`
	Models   *[]config.VertexCompatModel `json:"models"`
}

func (s *Service) VertexCompatKeys() []config.VertexCompatKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return s.cfg.VertexCompatAPIKey
}

func (s *Service) ReplaceVertexCompatKeys(entries []config.VertexCompatKey) {
	if s == nil || s.cfg == nil {
		return
	}
	for i := range entries {
		NormalizeVertexCompatKey(&entries[i])
	}
	prev := append([]config.VertexCompatKey(nil), s.cfg.VertexCompatAPIKey...)
	next := &config.Config{VertexCompatAPIKey: entries}
	prepareProviderStableIDs(&config.Config{VertexCompatAPIKey: prev}, next)
	s.cfg.VertexCompatAPIKey = next.VertexCompatAPIKey
	s.cfg.SanitizeVertexCompatKeys()
}

func (s *Service) PatchVertexCompatKey(index *int, match *string, patch VertexCompatPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.VertexCompatAPIKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && match != nil {
		matchValue := strings.TrimSpace(*match)
		if matchValue != "" {
			for i := range s.cfg.VertexCompatAPIKey {
				if s.cfg.VertexCompatAPIKey[i].APIKey == matchValue {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.VertexCompatAPIKey[targetIndex]
	if patch.APIKey != nil {
		trimmed := strings.TrimSpace(*patch.APIKey)
		if trimmed == "" {
			s.deleteVertexCompatKeyByIndex(targetIndex)
			return nil
		}
		entry.APIKey = trimmed
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
	}
	if patch.BaseURL != nil {
		trimmed := strings.TrimSpace(*patch.BaseURL)
		if trimmed == "" {
			s.deleteVertexCompatKeyByIndex(targetIndex)
			return nil
		}
		entry.BaseURL = trimmed
	}
	if patch.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*patch.ProxyURL)
	}
	if patch.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*patch.ProxyID)
	}
	if patch.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*patch.Headers)
	}
	if patch.Models != nil {
		entry.Models = append([]config.VertexCompatModel(nil), (*patch.Models)...)
	}
	NormalizeVertexCompatKey(&entry)
	s.cfg.VertexCompatAPIKey[targetIndex] = entry
	s.cfg.SanitizeVertexCompatKeys()
	return nil
}

func (s *Service) DeleteVertexCompatKeyByAPIKey(apiKey string) {
	if s == nil || s.cfg == nil {
		return
	}
	out := make([]config.VertexCompatKey, 0, len(s.cfg.VertexCompatAPIKey))
	for _, entry := range s.cfg.VertexCompatAPIKey {
		if entry.APIKey != apiKey {
			out = append(out, entry)
		}
	}
	s.cfg.VertexCompatAPIKey = out
	s.cfg.SanitizeVertexCompatKeys()
}

func (s *Service) DeleteVertexCompatKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.VertexCompatAPIKey) {
		return false
	}
	s.deleteVertexCompatKeyByIndex(index)
	return true
}

func (s *Service) deleteVertexCompatKeyByIndex(index int) {
	s.cfg.VertexCompatAPIKey = append(s.cfg.VertexCompatAPIKey[:index], s.cfg.VertexCompatAPIKey[index+1:]...)
	s.cfg.SanitizeVertexCompatKeys()
}

func NormalizeOpenAICompatibilityEntry(entry *config.OpenAICompatibility) {
	if entry == nil {
		return
	}
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	for i := range entry.APIKeyEntries {
		entry.APIKeyEntries[i].APIKey = strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
		entry.APIKeyEntries[i].ProxyURL = strings.TrimSpace(entry.APIKeyEntries[i].ProxyURL)
		entry.APIKeyEntries[i].ProxyID = strings.TrimSpace(entry.APIKeyEntries[i].ProxyID)
	}
}

func NormalizedOpenAICompatibilityEntries(entries []config.OpenAICompatibility) []config.OpenAICompatibility {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenAICompatibility, len(entries))
	for i := range entries {
		copyEntry := entries[i]
		if len(copyEntry.APIKeyEntries) > 0 {
			copyEntry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), copyEntry.APIKeyEntries...)
		}
		NormalizeOpenAICompatibilityEntry(&copyEntry)
		out[i] = copyEntry
	}
	return out
}

func NormalizeVertexCompatKey(entry *config.VertexCompatKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.VertexCompatModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" || model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}
