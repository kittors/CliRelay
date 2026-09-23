package providers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// OpenCode Go provider settings.
//
// Kept in its own file because provider_keys_extended.go is frozen at its
// current size by the structure ratchet and may only shrink.

type OpenCodeGoPatch struct {
	APIKey          *string                   `json:"api-key"`
	Name            *string                   `json:"name"`
	Disabled        *bool                     `json:"disabled"`
	Priority        *int                      `json:"priority"`
	Prefix          *string                   `json:"prefix"`
	ProxyURL        *string                   `json:"proxy-url"`
	ProxyID         *string                   `json:"proxy-id"`
	Headers         *map[string]string        `json:"headers"`
	Models          *[]config.OpenCodeGoModel `json:"models"`
	ExcludedModels  *[]string                 `json:"excluded-models"`
	VisionFallback  *string                   `json:"vision-fallback-model"`
	CodexToolBridge *bool                     `json:"codex-tool-bridge"`
}

func (s *Service) OpenCodeGoKeys() []config.OpenCodeGoKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizedOpenCodeGoKeyEntries(s.cfg.OpenCodeGoKey)
}

func (s *Service) ReplaceOpenCodeGoKeys(entries []config.OpenCodeGoKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	filtered := make([]config.OpenCodeGoKey, 0, len(entries))
	for i := range entries {
		NormalizeOpenCodeGoKey(&entries[i])
		if strings.TrimSpace(entries[i].APIKey) != "" {
			if err := validateOpenCodeGoKeyModels(entries[i]); err != nil {
				return err
			}
			filtered = append(filtered, entries[i])
		}
	}
	if len(entries) > 0 && len(filtered) == 0 {
		return ErrProviderAPIKeyRequired
	}
	prev := append([]config.OpenCodeGoKey(nil), s.cfg.OpenCodeGoKey...)
	next := &config.Config{OpenCodeGoKey: filtered}
	prepareProviderStableIDs(&config.Config{OpenCodeGoKey: prev}, next)
	s.cfg.OpenCodeGoKey = next.OpenCodeGoKey
	s.cfg.SanitizeOpenCodeGoKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.OpenCodeGoKey = prev
		return err
	}
	return nil
}

func (s *Service) PatchOpenCodeGoKey(index *int, apiKey *string, name *string, patch OpenCodeGoPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.OpenCodeGoKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && apiKey != nil {
		match := strings.TrimSpace(*apiKey)
		for i := range s.cfg.OpenCodeGoKey {
			if s.cfg.OpenCodeGoKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 && name != nil {
		match := strings.TrimSpace(*name)
		for i := range s.cfg.OpenCodeGoKey {
			if s.cfg.OpenCodeGoKey[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.OpenCodeGoKey[targetIndex]
	if patch.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*patch.APIKey)
	}
	if patch.Name != nil {
		entry.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Disabled != nil {
		entry.Disabled = *patch.Disabled
	}
	if patch.Priority != nil {
		entry.Priority = *patch.Priority
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
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
		entry.Models = append([]config.OpenCodeGoModel(nil), (*patch.Models)...)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	if patch.VisionFallback != nil {
		entry.VisionFallbackModel = strings.TrimSpace(*patch.VisionFallback)
	}
	if patch.CodexToolBridge != nil {
		entry.CodexToolBridge = *patch.CodexToolBridge
	}
	NormalizeOpenCodeGoKey(&entry)
	if entry.APIKey == "" {
		return ErrProviderAPIKeyRequired
	}
	if err := validateOpenCodeGoKeyModels(entry); err != nil {
		return err
	}
	prev := append([]config.OpenCodeGoKey(nil), s.cfg.OpenCodeGoKey...)
	s.cfg.OpenCodeGoKey[targetIndex] = entry
	s.cfg.SanitizeOpenCodeGoKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.OpenCodeGoKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteOpenCodeGoKeyByAPIKey(apiKey string) bool {
	return s.deleteOpenCodeGoKeys(func(entry config.OpenCodeGoKey) bool { return entry.APIKey == apiKey })
}

func (s *Service) DeleteOpenCodeGoKeyByName(name string) bool {
	return s.deleteOpenCodeGoKeys(func(entry config.OpenCodeGoKey) bool { return entry.Name == name })
}

func (s *Service) DeleteOpenCodeGoKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.OpenCodeGoKey) {
		return false
	}
	s.deleteOpenCodeGoKeyByIndex(index)
	return true
}

func (s *Service) deleteOpenCodeGoKeys(match func(config.OpenCodeGoKey) bool) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.OpenCodeGoKey, 0, len(s.cfg.OpenCodeGoKey))
	for _, entry := range s.cfg.OpenCodeGoKey {
		if !match(entry) {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.OpenCodeGoKey) {
		return false
	}
	s.cfg.OpenCodeGoKey = out
	s.cfg.SanitizeOpenCodeGoKeys()
	return true
}

func (s *Service) deleteOpenCodeGoKeyByIndex(index int) {
	s.cfg.OpenCodeGoKey = append(s.cfg.OpenCodeGoKey[:index], s.cfg.OpenCodeGoKey[index+1:]...)
	s.cfg.SanitizeOpenCodeGoKeys()
}

func NormalizeOpenCodeGoKey(entry *config.OpenCodeGoKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.Models = config.NormalizeOpenCodeGoModels(entry.Models)
	entry.ExcludedModels = config.NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
}

func NormalizedOpenCodeGoKeyEntries(entries []config.OpenCodeGoKey) []config.OpenCodeGoKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenCodeGoKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeOpenCodeGoKey(&out[i])
		if config.IsProviderModelAccessDisabledAll(out[i].ExcludedModels) {
			out[i].Models = nil
		}
	}
	return out
}
