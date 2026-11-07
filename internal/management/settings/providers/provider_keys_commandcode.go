package providers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/patchfield"
)

// Command Code provider settings.
//
// Kept in its own file because provider_keys_extended.go is frozen at its
// current size by the structure ratchet and may only shrink.
//
// Unlike Cline, Ollama Cloud and OpenCode Go, this channel carries no dashboard
// cookie: Command Code reports plan windows and credits from an endpoint that
// authenticates with the same API key used for inference, so there is nothing
// for the operator to paste from a browser and nothing here that expires.

type CommandCodePatch struct {
	APIKey         *string                    `json:"api-key"`
	Name           *string                    `json:"name"`
	Disabled       *bool                      `json:"disabled"`
	Priority       *int                       `json:"priority"`
	Prefix         *string                    `json:"prefix"`
	BaseURL        *string                    `json:"base-url"`
	ProxyURL       *string                    `json:"proxy-url"`
	ProxyID        *string                    `json:"proxy-id"`
	Headers        *map[string]string         `json:"headers"`
	Models         *[]config.CommandCodeModel `json:"models"`
	ExcludedModels *[]string                  `json:"excluded-models"`
	VisionFallback *string                    `json:"vision-fallback-model"`
	DisableCooling patchfield.NullableBool    `json:"disable-cooling"`
}

func (s *Service) CommandCodeKeys() []config.CommandCodeKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizedCommandCodeKeyEntries(s.cfg.CommandCodeKey)
}

func (s *Service) ReplaceCommandCodeKeys(entries []config.CommandCodeKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	filtered := make([]config.CommandCodeKey, 0, len(entries))
	for i := range entries {
		NormalizeCommandCodeKey(&entries[i])
		if strings.TrimSpace(entries[i].APIKey) != "" {
			if err := validateCommandCodeKeyModels(entries[i]); err != nil {
				return err
			}
			filtered = append(filtered, entries[i])
		}
	}
	if len(entries) > 0 && len(filtered) == 0 {
		return ErrProviderAPIKeyRequired
	}
	prev := append([]config.CommandCodeKey(nil), s.cfg.CommandCodeKey...)
	next := &config.Config{CommandCodeKey: filtered}
	prepareProviderStableIDs(&config.Config{CommandCodeKey: prev}, next)
	s.cfg.CommandCodeKey = next.CommandCodeKey
	s.cfg.SanitizeCommandCodeKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.CommandCodeKey = prev
		return err
	}
	return nil
}

func (s *Service) PatchCommandCodeKey(index *int, apiKey *string, name *string, patch CommandCodePatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.CommandCodeKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && apiKey != nil {
		match := strings.TrimSpace(*apiKey)
		for i := range s.cfg.CommandCodeKey {
			if s.cfg.CommandCodeKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 && name != nil {
		match := strings.TrimSpace(*name)
		for i := range s.cfg.CommandCodeKey {
			if s.cfg.CommandCodeKey[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.CommandCodeKey[targetIndex]
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
	if patch.Models != nil {
		entry.Models = append([]config.CommandCodeModel(nil), (*patch.Models)...)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	if patch.VisionFallback != nil {
		entry.VisionFallbackModel = strings.TrimSpace(*patch.VisionFallback)
	}
	patch.DisableCooling.Apply(&entry.DisableCooling)
	NormalizeCommandCodeKey(&entry)
	if entry.APIKey == "" {
		return ErrProviderAPIKeyRequired
	}
	if err := validateCommandCodeKeyModels(entry); err != nil {
		return err
	}
	prev := append([]config.CommandCodeKey(nil), s.cfg.CommandCodeKey...)
	s.cfg.CommandCodeKey[targetIndex] = entry
	s.cfg.SanitizeCommandCodeKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.CommandCodeKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteCommandCodeKeyByAPIKey(apiKey string) bool {
	return s.deleteCommandCodeKeys(func(entry config.CommandCodeKey) bool { return entry.APIKey == apiKey })
}

func (s *Service) DeleteCommandCodeKeyByName(name string) bool {
	return s.deleteCommandCodeKeys(func(entry config.CommandCodeKey) bool { return entry.Name == name })
}

func (s *Service) DeleteCommandCodeKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.CommandCodeKey) {
		return false
	}
	s.deleteCommandCodeKeyByIndex(index)
	return true
}

func (s *Service) deleteCommandCodeKeys(match func(config.CommandCodeKey) bool) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.CommandCodeKey, 0, len(s.cfg.CommandCodeKey))
	for _, entry := range s.cfg.CommandCodeKey {
		if !match(entry) {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.CommandCodeKey) {
		return false
	}
	s.cfg.CommandCodeKey = out
	s.cfg.SanitizeCommandCodeKeys()
	return true
}

func (s *Service) deleteCommandCodeKeyByIndex(index int) {
	s.cfg.CommandCodeKey = append(s.cfg.CommandCodeKey[:index], s.cfg.CommandCodeKey[index+1:]...)
	s.cfg.SanitizeCommandCodeKeys()
}

func NormalizeCommandCodeKey(entry *config.CommandCodeKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSuffix(strings.TrimSpace(entry.BaseURL), "/")
	if entry.BaseURL == "" {
		entry.BaseURL = config.DefaultCommandCodeBaseURL
	}
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.Models = config.NormalizeCommandCodeModels(entry.Models)
	entry.ExcludedModels = config.NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
}

func NormalizedCommandCodeKeyEntries(entries []config.CommandCodeKey) []config.CommandCodeKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.CommandCodeKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeCommandCodeKey(&out[i])
		if config.IsProviderModelAccessDisabledAll(out[i].ExcludedModels) {
			out[i].Models = nil
		}
	}
	return out
}

// validateCommandCodeKeyModels keeps cline-pass IDs out of this channel; those
// belong to Cline and would otherwise be routed to the wrong upstream.
func validateCommandCodeKeyModels(entry config.CommandCodeKey) error {
	for _, model := range entry.Models {
		if isClinePassModelID(model.Name) {
			return providerModelOwnershipError("commandcode", "models", model.Name, "must not use cline-pass model IDs")
		}
	}
	for _, model := range entry.ExcludedModels {
		if model == "*" {
			continue
		}
		if isClinePassModelID(model) {
			return providerModelOwnershipError("commandcode", "excluded-models", model, "must not use cline-pass model IDs")
		}
	}
	return nil
}
