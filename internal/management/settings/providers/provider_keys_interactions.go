package providers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// InteractionsKeys returns the native Google Interactions credentials.
func (s *Service) InteractionsKeys() []config.GeminiKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return s.cfg.InteractionsKey
}

// ReplaceInteractionsKeys replaces the native Google Interactions credentials.
func (s *Service) ReplaceInteractionsKeys(entries []config.GeminiKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	prev := append([]config.GeminiKey(nil), s.cfg.InteractionsKey...)
	next := &config.Config{InteractionsKey: append([]config.GeminiKey(nil), entries...)}
	prepareProviderStableIDs(&config.Config{InteractionsKey: prev}, next)
	s.cfg.InteractionsKey = next.InteractionsKey
	s.cfg.SanitizeInteractionsKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.InteractionsKey = prev
		return err
	}
	return nil
}

// PatchInteractionsKey updates one native Google Interactions credential.
func (s *Service) PatchInteractionsKey(index *int, match *string, patch GeminiKeyPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.InteractionsKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && match != nil {
		matchValue := strings.TrimSpace(*match)
		if matchValue != "" {
			for i := range s.cfg.InteractionsKey {
				if strings.TrimSpace(s.cfg.InteractionsKey[i].APIKey) == matchValue {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.InteractionsKey[targetIndex]
	if patch.APIKey != nil {
		trimmed := strings.TrimSpace(*patch.APIKey)
		if trimmed == "" {
			s.deleteInteractionsKeyByIndex(targetIndex)
			return nil
		}
		entry.APIKey = trimmed
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
	}
	if patch.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*patch.BaseURL)
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
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	patch.DisableCooling.Apply(&entry.DisableCooling)
	prev := append([]config.GeminiKey(nil), s.cfg.InteractionsKey...)
	s.cfg.InteractionsKey[targetIndex] = entry
	s.cfg.SanitizeInteractionsKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.InteractionsKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteInteractionsKeyByAPIKey(apiKey string) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.GeminiKey, 0, len(s.cfg.InteractionsKey))
	for _, entry := range s.cfg.InteractionsKey {
		if entry.APIKey != apiKey {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.InteractionsKey) {
		return false
	}
	s.cfg.InteractionsKey = out
	s.cfg.SanitizeInteractionsKeys()
	return true
}

func (s *Service) DeleteInteractionsKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.InteractionsKey) {
		return false
	}
	s.deleteInteractionsKeyByIndex(index)
	return true
}

func (s *Service) deleteInteractionsKeyByIndex(index int) {
	s.cfg.InteractionsKey = append(s.cfg.InteractionsKey[:index], s.cfg.InteractionsKey[index+1:]...)
	s.cfg.SanitizeInteractionsKeys()
}
