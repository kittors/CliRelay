package providers

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var openCodeGoServerIDPattern = regexp.MustCompile(`(?i)^[a-f0-9]{64}$`)

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
	s.cfg.BedrockKey = normalized
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

type OpenCodeGoPatch struct {
	APIKey         *string                   `json:"api-key"`
	Name           *string                   `json:"name"`
	Priority       *int                      `json:"priority"`
	Prefix         *string                   `json:"prefix"`
	ProxyURL       *string                   `json:"proxy-url"`
	ProxyID        *string                   `json:"proxy-id"`
	Headers        *map[string]string        `json:"headers"`
	Models         *[]config.OpenCodeGoModel `json:"models"`
	ExcludedModels *[]string                 `json:"excluded-models"`
	VisionFallback *string                   `json:"vision-fallback-model"`
	WorkspaceID    *string                   `json:"workspace-id"`
	AuthCookie     *string                   `json:"auth-cookie"`
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
	prev := append([]config.OpenCodeGoKey(nil), s.cfg.OpenCodeGoKey...)
	s.cfg.OpenCodeGoKey = filtered
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
	if patch.WorkspaceID != nil {
		entry.WorkspaceID = strings.TrimSpace(*patch.WorkspaceID)
	}
	if patch.AuthCookie != nil {
		entry.AuthCookie = strings.TrimSpace(*patch.AuthCookie)
	}
	NormalizeOpenCodeGoKey(&entry)
	if entry.APIKey == "" {
		s.deleteOpenCodeGoKeyByIndex(targetIndex)
		return nil
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

type ClinePatch struct {
	APIKey         *string              `json:"api-key"`
	Name           *string              `json:"name"`
	Priority       *int                 `json:"priority"`
	Prefix         *string              `json:"prefix"`
	BaseURL        *string              `json:"base-url"`
	ProxyURL       *string              `json:"proxy-url"`
	ProxyID        *string              `json:"proxy-id"`
	Headers        *map[string]string   `json:"headers"`
	Models         *[]config.ClineModel `json:"models"`
	ExcludedModels *[]string            `json:"excluded-models"`
	VisionFallback *string              `json:"vision-fallback-model"`
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
	prev := append([]config.ClineKey(nil), s.cfg.ClineKey...)
	s.cfg.ClineKey = filtered
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
	NormalizeClineKey(&entry)
	if entry.APIKey == "" {
		s.deleteClineKeyByIndex(targetIndex)
		return nil
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
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
	if workspaceID, err := normalizeOpenCodeGoWorkspaceID(entry.WorkspaceID); err == nil {
		entry.WorkspaceID = workspaceID
	} else {
		entry.WorkspaceID = strings.TrimSpace(entry.WorkspaceID)
	}
	entry.AuthCookie = strings.TrimSpace(entry.AuthCookie)
}

func NormalizedOpenCodeGoKeyEntries(entries []config.OpenCodeGoKey) []config.OpenCodeGoKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenCodeGoKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeOpenCodeGoKey(&out[i])
	}
	return out
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
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
}

func NormalizedClineKeyEntries(entries []config.ClineKey) []config.ClineKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.ClineKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		NormalizeClineKey(&out[i])
	}
	return out
}

func normalizeOpenCodeGoWorkspaceID(raw string) (string, error) {
	raw = strings.Trim(strings.TrimSpace(raw), `"'`)
	if raw == "" {
		return "", nil
	}
	if id := extractOpenCodeGoWorkspaceID(raw); id != "" {
		return id, nil
	}
	trimmed := strings.Trim(raw, "/")
	if strings.EqualFold(trimmed, "default") || openCodeGoServerIDPattern.MatchString(trimmed) {
		return trimmed, errors.New("invalid workspace id")
	}
	return trimmed, nil
}

func extractOpenCodeGoWorkspaceID(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Path != "" {
		if id := extractOpenCodeGoWorkspaceIDFromPath(parsed.Path); id != "" {
			return id
		}
	}
	return extractOpenCodeGoWorkspaceIDFromPath(raw)
}

func extractOpenCodeGoWorkspaceIDFromPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part != "workspace" || i+1 >= len(parts) {
			continue
		}
		id := strings.TrimSpace(parts[i+1])
		if id == "" {
			continue
		}
		if unescaped, err := url.PathUnescape(id); err == nil {
			id = unescaped
		}
		id = strings.Trim(strings.TrimSpace(id), `"'`)
		if id != "" {
			return id
		}
	}
	return ""
}
