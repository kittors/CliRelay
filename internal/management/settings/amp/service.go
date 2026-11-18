package amp

import (
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var ErrEmptyValue = errors.New("empty value")

type Service struct {
	cfg *config.Config
}

func NewService(cfg *config.Config) *Service {
	return &Service{cfg: cfg}
}

func (s *Service) Snapshot() config.AmpCode {
	if s == nil || s.cfg == nil {
		return config.AmpCode{}
	}
	return s.cfg.AmpCode
}

func (s *Service) UpstreamURL() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.AmpCode.UpstreamURL
}

func (s *Service) SetUpstreamURL(value string) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.UpstreamURL = strings.TrimSpace(value)
}

func (s *Service) ClearUpstreamURL() {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.UpstreamURL = ""
}

func (s *Service) UpstreamAPIKey() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.AmpCode.UpstreamAPIKey
}

func (s *Service) SetUpstreamAPIKey(value string) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.UpstreamAPIKey = strings.TrimSpace(value)
}

func (s *Service) ClearUpstreamAPIKey() {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.UpstreamAPIKey = ""
}

func (s *Service) RestrictManagementToLocalhost() bool {
	if s == nil || s.cfg == nil {
		return true
	}
	return s.cfg.AmpCode.RestrictManagementToLocalhost
}

func (s *Service) SetRestrictManagementToLocalhost(value bool) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.RestrictManagementToLocalhost = value
}

func (s *Service) ModelMappings() []config.AmpModelMapping {
	if s == nil || s.cfg == nil {
		return []config.AmpModelMapping{}
	}
	return s.cfg.AmpCode.ModelMappings
}

func (s *Service) SetModelMappings(value []config.AmpModelMapping) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.ModelMappings = value
}

func (s *Service) PatchModelMappings(value []config.AmpModelMapping) {
	if s == nil || s.cfg == nil {
		return
	}
	existing := make(map[string]int)
	for i, mapping := range s.cfg.AmpCode.ModelMappings {
		existing[strings.TrimSpace(mapping.From)] = i
	}

	for _, newMapping := range value {
		from := strings.TrimSpace(newMapping.From)
		if idx, ok := existing[from]; ok {
			s.cfg.AmpCode.ModelMappings[idx] = newMapping
			continue
		}
		s.cfg.AmpCode.ModelMappings = append(s.cfg.AmpCode.ModelMappings, newMapping)
		existing[from] = len(s.cfg.AmpCode.ModelMappings) - 1
	}
}

func (s *Service) DeleteModelMappings(value []string) {
	if s == nil || s.cfg == nil {
		return
	}
	if len(value) == 0 {
		s.cfg.AmpCode.ModelMappings = nil
		return
	}

	toRemove := make(map[string]bool)
	for _, from := range value {
		toRemove[strings.TrimSpace(from)] = true
	}

	newMappings := make([]config.AmpModelMapping, 0, len(s.cfg.AmpCode.ModelMappings))
	for _, mapping := range s.cfg.AmpCode.ModelMappings {
		if !toRemove[strings.TrimSpace(mapping.From)] {
			newMappings = append(newMappings, mapping)
		}
	}
	s.cfg.AmpCode.ModelMappings = newMappings
}

func (s *Service) ForceModelMappings() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return s.cfg.AmpCode.ForceModelMappings
}

func (s *Service) SetForceModelMappings(value bool) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.ForceModelMappings = value
}

func (s *Service) UpstreamAPIKeys() []config.AmpUpstreamAPIKeyEntry {
	if s == nil || s.cfg == nil {
		return []config.AmpUpstreamAPIKeyEntry{}
	}
	return s.cfg.AmpCode.UpstreamAPIKeys
}

func (s *Service) SetUpstreamAPIKeys(value []config.AmpUpstreamAPIKeyEntry) {
	if s == nil || s.cfg == nil {
		return
	}
	s.cfg.AmpCode.UpstreamAPIKeys = normalizeUpstreamAPIKeyEntries(value)
}

func (s *Service) PatchUpstreamAPIKeys(value []config.AmpUpstreamAPIKeyEntry) {
	if s == nil || s.cfg == nil {
		return
	}
	existing := make(map[string]int)
	for i, entry := range s.cfg.AmpCode.UpstreamAPIKeys {
		existing[strings.TrimSpace(entry.UpstreamAPIKey)] = i
	}

	for _, newEntry := range value {
		upstreamKey := strings.TrimSpace(newEntry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		normalizedEntry := config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        normalizeAPIKeys(newEntry.APIKeys),
		}
		if idx, ok := existing[upstreamKey]; ok {
			s.cfg.AmpCode.UpstreamAPIKeys[idx] = normalizedEntry
			continue
		}
		s.cfg.AmpCode.UpstreamAPIKeys = append(s.cfg.AmpCode.UpstreamAPIKeys, normalizedEntry)
		existing[upstreamKey] = len(s.cfg.AmpCode.UpstreamAPIKeys) - 1
	}
}

func (s *Service) DeleteUpstreamAPIKeys(value []string) error {
	if s == nil || s.cfg == nil {
		return nil
	}
	if len(value) == 0 {
		s.cfg.AmpCode.UpstreamAPIKeys = nil
		return nil
	}

	toRemove := make(map[string]bool)
	for _, key := range value {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		toRemove[trimmed] = true
	}
	if len(toRemove) == 0 {
		return ErrEmptyValue
	}

	newEntries := make([]config.AmpUpstreamAPIKeyEntry, 0, len(s.cfg.AmpCode.UpstreamAPIKeys))
	for _, entry := range s.cfg.AmpCode.UpstreamAPIKeys {
		if !toRemove[strings.TrimSpace(entry.UpstreamAPIKey)] {
			newEntries = append(newEntries, entry)
		}
	}
	s.cfg.AmpCode.UpstreamAPIKeys = newEntries
	return nil
}

func normalizeUpstreamAPIKeyEntries(entries []config.AmpUpstreamAPIKeyEntry) []config.AmpUpstreamAPIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.AmpUpstreamAPIKeyEntry, 0, len(entries))
	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		out = append(out, config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        normalizeAPIKeys(entry.APIKeys),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAPIKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
