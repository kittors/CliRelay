package providers

import (
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var ErrProviderAPIKeyRequired = errors.New("api-key is required")

type BedrockKeyPatch struct {
	Name            *string                `json:"name"`
	Priority        *int                   `json:"priority"`
	Prefix          *string                `json:"prefix"`
	AuthMode        *string                `json:"auth-mode"`
	APIKey          *string                `json:"api-key"`
	AccessKeyID     *string                `json:"access-key-id"`
	SecretAccessKey *string                `json:"secret-access-key"`
	SessionToken    *string                `json:"session-token"`
	Region          *string                `json:"region"`
	ForceGlobal     *bool                  `json:"force-global"`
	BaseURL         *string                `json:"base-url"`
	ProxyURL        *string                `json:"proxy-url"`
	ProxyID         *string                `json:"proxy-id"`
	Models          *[]config.BedrockModel `json:"models"`
	Headers         *map[string]string     `json:"headers"`
	ExcludedModels  *[]string              `json:"excluded-models"`
}

func (s *Service) BedrockKeys() []config.BedrockKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return s.cfg.BedrockKey
}

func (s *Service) ReplaceBedrockKeys(entries []config.BedrockKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	normalized := append([]config.BedrockKey(nil), entries...)
	for i := range normalized {
		NormalizeBedrockKey(&normalized[i])
	}
	prev := append([]config.BedrockKey(nil), s.cfg.BedrockKey...)
	next := &config.Config{BedrockKey: normalized}
	prepareProviderStableIDs(&config.Config{BedrockKey: prev}, next)
	s.cfg.BedrockKey = next.BedrockKey
	s.cfg.SanitizeBedrockKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.BedrockKey = prev
		return err
	}
	return nil
}

func (s *Service) PatchBedrockKey(index *int, match *string, patch BedrockKeyPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.BedrockKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && match != nil {
		matchValue := strings.TrimSpace(*match)
		for i := range s.cfg.BedrockKey {
			entry := s.cfg.BedrockKey[i]
			if entry.APIKey == matchValue || entry.AccessKeyID == matchValue || entry.Name == matchValue {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.BedrockKey[targetIndex]
	if patch.Name != nil {
		entry.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Priority != nil {
		entry.Priority = *patch.Priority
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
	}
	if patch.AuthMode != nil {
		entry.AuthMode = strings.TrimSpace(*patch.AuthMode)
	}
	if patch.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*patch.APIKey)
	}
	if patch.AccessKeyID != nil {
		entry.AccessKeyID = strings.TrimSpace(*patch.AccessKeyID)
	}
	if patch.SecretAccessKey != nil {
		entry.SecretAccessKey = strings.TrimSpace(*patch.SecretAccessKey)
	}
	if patch.SessionToken != nil {
		entry.SessionToken = strings.TrimSpace(*patch.SessionToken)
	}
	if patch.Region != nil {
		entry.Region = strings.TrimSpace(*patch.Region)
	}
	if patch.ForceGlobal != nil {
		entry.ForceGlobal = *patch.ForceGlobal
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
	if patch.Models != nil {
		entry.Models = append([]config.BedrockModel(nil), (*patch.Models)...)
	}
	if patch.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*patch.Headers)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	NormalizeBedrockKey(&entry)
	prev := append([]config.BedrockKey(nil), s.cfg.BedrockKey...)
	s.cfg.BedrockKey[targetIndex] = entry
	s.cfg.SanitizeBedrockKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.BedrockKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteBedrockKeyByAPIKey(apiKey string) bool {
	return s.deleteBedrockKeys(func(entry config.BedrockKey) bool { return entry.APIKey == apiKey })
}

func (s *Service) DeleteBedrockKeyByAccessKeyID(accessKeyID string) bool {
	return s.deleteBedrockKeys(func(entry config.BedrockKey) bool { return entry.AccessKeyID == accessKeyID })
}

func (s *Service) DeleteBedrockKeyByName(name string) bool {
	return s.deleteBedrockKeys(func(entry config.BedrockKey) bool { return entry.Name == name })
}

func (s *Service) DeleteBedrockKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.BedrockKey) {
		return false
	}
	s.cfg.BedrockKey = append(s.cfg.BedrockKey[:index], s.cfg.BedrockKey[index+1:]...)
	s.cfg.SanitizeBedrockKeys()
	return true
}

func (s *Service) deleteBedrockKeys(match func(config.BedrockKey) bool) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.BedrockKey, 0, len(s.cfg.BedrockKey))
	for _, entry := range s.cfg.BedrockKey {
		if !match(entry) {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.BedrockKey) {
		return false
	}
	s.cfg.BedrockKey = out
	s.cfg.SanitizeBedrockKeys()
	return true
}

type ClinePatch struct {
	APIKey         *string              `json:"api-key"`
	Name           *string              `json:"name"`
	Disabled       *bool                `json:"disabled"`
	Priority       *int                 `json:"priority"`
	Prefix         *string              `json:"prefix"`
	BaseURL        *string              `json:"base-url"`
	ProxyURL       *string              `json:"proxy-url"`
	ProxyID        *string              `json:"proxy-id"`
	Headers        *map[string]string   `json:"headers"`
	Models         *[]config.ClineModel `json:"models"`
	ExcludedModels *[]string            `json:"excluded-models"`
	VisionFallback *string              `json:"vision-fallback-model"`
	AuthCookie     *string              `json:"auth-cookie"`
}

func (s *Service) ClineKeys() []config.ClineKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizedClineKeyEntries(s.cfg.ClineKey)
}

func (s *Service) ReplaceClineKeys(entries []config.ClineKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	filtered := make([]config.ClineKey, 0, len(entries))
	for i := range entries {
		NormalizeClineKey(&entries[i])
		if strings.TrimSpace(entries[i].APIKey) != "" {
			if err := validateClineKeyModels(entries[i]); err != nil {
				return err
			}
			filtered = append(filtered, entries[i])
		}
	}
	if len(entries) > 0 && len(filtered) == 0 {
		return ErrProviderAPIKeyRequired
	}
	prev := append([]config.ClineKey(nil), s.cfg.ClineKey...)
	next := &config.Config{ClineKey: filtered}
	prepareProviderStableIDs(&config.Config{ClineKey: prev}, next)
	s.cfg.ClineKey = next.ClineKey
	s.cfg.SanitizeClineKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.ClineKey = prev
		return err
	}
	return nil
}

func (s *Service) PatchClineKey(index *int, apiKey *string, name *string, patch ClinePatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.ClineKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && apiKey != nil {
		match := strings.TrimSpace(*apiKey)
		for i := range s.cfg.ClineKey {
			if s.cfg.ClineKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 && name != nil {
		match := strings.TrimSpace(*name)
		for i := range s.cfg.ClineKey {
			if s.cfg.ClineKey[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.ClineKey[targetIndex]
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
		entry.Models = append([]config.ClineModel(nil), (*patch.Models)...)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	if patch.VisionFallback != nil {
		entry.VisionFallbackModel = strings.TrimSpace(*patch.VisionFallback)
	}
	if patch.AuthCookie != nil {
		entry.AuthCookie = strings.TrimSpace(*patch.AuthCookie)
	}
	NormalizeClineKey(&entry)
	if entry.APIKey == "" {
		return ErrProviderAPIKeyRequired
	}
	if err := validateClineKeyModels(entry); err != nil {
		return err
	}
	prev := append([]config.ClineKey(nil), s.cfg.ClineKey...)
	s.cfg.ClineKey[targetIndex] = entry
	s.cfg.SanitizeClineKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.ClineKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteClineKeyByAPIKey(apiKey string) bool {
	return s.deleteClineKeys(func(entry config.ClineKey) bool { return entry.APIKey == apiKey })
}

func (s *Service) DeleteClineKeyByName(name string) bool {
	return s.deleteClineKeys(func(entry config.ClineKey) bool { return entry.Name == name })
}

func (s *Service) DeleteClineKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.ClineKey) {
		return false
	}
	s.deleteClineKeyByIndex(index)
	return true
}

func (s *Service) deleteClineKeys(match func(config.ClineKey) bool) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.ClineKey, 0, len(s.cfg.ClineKey))
	for _, entry := range s.cfg.ClineKey {
		if !match(entry) {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.ClineKey) {
		return false
	}
	s.cfg.ClineKey = out
	s.cfg.SanitizeClineKeys()
	return true
}

func (s *Service) deleteClineKeyByIndex(index int) {
	s.cfg.ClineKey = append(s.cfg.ClineKey[:index], s.cfg.ClineKey[index+1:]...)
	s.cfg.SanitizeClineKeys()
}

type OllamaCloudPatch struct {
	APIKey         *string                    `json:"api-key"`
	Name           *string                    `json:"name"`
	Disabled       *bool                      `json:"disabled"`
	Priority       *int                       `json:"priority"`
	Prefix         *string                    `json:"prefix"`
	BaseURL        *string                    `json:"base-url"`
	ProxyURL       *string                    `json:"proxy-url"`
	ProxyID        *string                    `json:"proxy-id"`
	Headers        *map[string]string         `json:"headers"`
	Models         *[]config.OllamaCloudModel `json:"models"`
	ExcludedModels *[]string                  `json:"excluded-models"`
	VisionFallback *string                    `json:"vision-fallback-model"`
	AuthCookie     *string                    `json:"auth-cookie"`
}

func (s *Service) OllamaCloudKeys() []config.OllamaCloudKey {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizedOllamaCloudKeyEntries(s.cfg.OllamaCloudKey)
}

func (s *Service) ReplaceOllamaCloudKeys(entries []config.OllamaCloudKey) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	filtered := make([]config.OllamaCloudKey, 0, len(entries))
	for i := range entries {
		NormalizeOllamaCloudKey(&entries[i])
		if strings.TrimSpace(entries[i].APIKey) != "" {
			if err := validateOllamaCloudKeyModels(entries[i]); err != nil {
				return err
			}
			filtered = append(filtered, entries[i])
		}
	}
	if len(entries) > 0 && len(filtered) == 0 {
		return ErrProviderAPIKeyRequired
	}
	prev := append([]config.OllamaCloudKey(nil), s.cfg.OllamaCloudKey...)
	next := &config.Config{OllamaCloudKey: filtered}
	prepareProviderStableIDs(&config.Config{OllamaCloudKey: prev}, next)
	s.cfg.OllamaCloudKey = next.OllamaCloudKey
	s.cfg.SanitizeOllamaCloudKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.OllamaCloudKey = prev
		return err
	}
	return nil
}

func (s *Service) PatchOllamaCloudKey(index *int, apiKey *string, name *string, patch OllamaCloudPatch) error {
	if s == nil || s.cfg == nil {
		return ErrItemNotFound
	}
	targetIndex := -1
	if index != nil && *index >= 0 && *index < len(s.cfg.OllamaCloudKey) {
		targetIndex = *index
	}
	if targetIndex == -1 && apiKey != nil {
		match := strings.TrimSpace(*apiKey)
		for i := range s.cfg.OllamaCloudKey {
			if s.cfg.OllamaCloudKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 && name != nil {
		match := strings.TrimSpace(*name)
		for i := range s.cfg.OllamaCloudKey {
			if s.cfg.OllamaCloudKey[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		return ErrItemNotFound
	}

	entry := s.cfg.OllamaCloudKey[targetIndex]
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
		entry.Models = append([]config.OllamaCloudModel(nil), (*patch.Models)...)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	if patch.VisionFallback != nil {
		entry.VisionFallbackModel = strings.TrimSpace(*patch.VisionFallback)
	}
	if patch.AuthCookie != nil {
		entry.AuthCookie = strings.TrimSpace(*patch.AuthCookie)
	}
	NormalizeOllamaCloudKey(&entry)
	if entry.APIKey == "" {
		return ErrProviderAPIKeyRequired
	}
	if err := validateOllamaCloudKeyModels(entry); err != nil {
		return err
	}
	prev := append([]config.OllamaCloudKey(nil), s.cfg.OllamaCloudKey...)
	s.cfg.OllamaCloudKey[targetIndex] = entry
	s.cfg.SanitizeOllamaCloudKeys()
	if err := s.runValidator(); err != nil {
		s.cfg.OllamaCloudKey = prev
		return err
	}
	return nil
}

func (s *Service) DeleteOllamaCloudKeyByAPIKey(apiKey string) bool {
	return s.deleteOllamaCloudKeys(func(entry config.OllamaCloudKey) bool { return entry.APIKey == apiKey })
}

func (s *Service) DeleteOllamaCloudKeyByName(name string) bool {
	return s.deleteOllamaCloudKeys(func(entry config.OllamaCloudKey) bool { return entry.Name == name })
}

func (s *Service) DeleteOllamaCloudKeyByIndex(index int) bool {
	if s == nil || s.cfg == nil || index < 0 || index >= len(s.cfg.OllamaCloudKey) {
		return false
	}
	s.deleteOllamaCloudKeyByIndex(index)
	return true
}

func (s *Service) deleteOllamaCloudKeys(match func(config.OllamaCloudKey) bool) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	out := make([]config.OllamaCloudKey, 0, len(s.cfg.OllamaCloudKey))
	for _, entry := range s.cfg.OllamaCloudKey {
		if !match(entry) {
			out = append(out, entry)
		}
	}
	if len(out) == len(s.cfg.OllamaCloudKey) {
		return false
	}
	s.cfg.OllamaCloudKey = out
	s.cfg.SanitizeOllamaCloudKeys()
	return true
}

func (s *Service) deleteOllamaCloudKeyByIndex(index int) {
	s.cfg.OllamaCloudKey = append(s.cfg.OllamaCloudKey[:index], s.cfg.OllamaCloudKey[index+1:]...)
	s.cfg.SanitizeOllamaCloudKeys()
}

func NormalizeBedrockKey(entry *config.BedrockKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.AuthMode = strings.TrimSpace(entry.AuthMode)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.AccessKeyID = strings.TrimSpace(entry.AccessKeyID)
	entry.SecretAccessKey = strings.TrimSpace(entry.SecretAccessKey)
	entry.SessionToken = strings.TrimSpace(entry.SessionToken)
	entry.Region = strings.TrimSpace(entry.Region)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.BedrockModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func NormalizeClineKey(entry *config.ClineKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSuffix(strings.TrimSpace(entry.BaseURL), "/")
	if entry.BaseURL == "" {
		entry.BaseURL = config.DefaultClineBaseURL
	}
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.Models = config.NormalizeClineModels(entry.Models)
	entry.ExcludedModels = config.NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
	entry.AuthCookie = strings.TrimSpace(entry.AuthCookie)
}

func NormalizedClineKeyEntries(entries []config.ClineKey) []config.ClineKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.ClineKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeClineKey(&out[i])
		if config.IsProviderModelAccessDisabledAll(out[i].ExcludedModels) {
			out[i].Models = nil
		}
	}
	return out
}

func NormalizeOllamaCloudKey(entry *config.OllamaCloudKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSuffix(strings.TrimSpace(entry.BaseURL), "/")
	if entry.BaseURL == "" {
		entry.BaseURL = config.DefaultOllamaCloudBaseURL
	}
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.Models = config.NormalizeOllamaCloudModels(entry.Models)
	entry.ExcludedModels = config.NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
	entry.AuthCookie = strings.TrimSpace(entry.AuthCookie)
}

func NormalizedOllamaCloudKeyEntries(entries []config.OllamaCloudKey) []config.OllamaCloudKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OllamaCloudKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeOllamaCloudKey(&out[i])
		if config.IsProviderModelAccessDisabledAll(out[i].ExcludedModels) {
			out[i].Models = nil
		}
	}
	return out
}
