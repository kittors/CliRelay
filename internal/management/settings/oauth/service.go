package oauth

import (
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var (
	ErrInvalidProvider  = errors.New("invalid provider")
	ErrProviderNotFound = errors.New("provider not found")
	ErrInvalidChannel   = errors.New("invalid channel")
	ErrChannelNotFound  = errors.New("channel not found")
)

type Service struct {
	cfg *config.Config
}

func NewService(cfg *config.Config) *Service {
	return &Service{cfg: cfg}
}

func NormalizeModelAlias(entries map[string][]config.OAuthModelAlias) map[string][]config.OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	copied := make(map[string][]config.OAuthModelAlias, len(entries))
	for channel, aliases := range entries {
		if len(aliases) == 0 {
			continue
		}
		copied[channel] = append([]config.OAuthModelAlias(nil), aliases...)
	}
	if len(copied) == 0 {
		return nil
	}
	cfg := config.Config{OAuthModelAlias: copied}
	cfg.SanitizeOAuthModelAlias()
	if len(cfg.OAuthModelAlias) == 0 {
		return nil
	}
	return cfg.OAuthModelAlias
}

func (s *Service) ExcludedModels() map[string][]string {
	if s == nil || s.cfg == nil {
		return nil
	}
	return config.NormalizeOAuthExcludedModels(s.cfg.OAuthExcludedModels)
}

func (s *Service) SetExcludedModels(entries map[string][]string) map[string][]string {
	normalized := config.NormalizeOAuthExcludedModels(entries)
	if s == nil || s.cfg == nil {
		return normalized
	}
	s.cfg.OAuthExcludedModels = normalized
	return s.cfg.OAuthExcludedModels
}

func (s *Service) PatchExcludedModels(provider string, models []string) (map[string][]string, error) {
	provider = normalizeKey(provider)
	if provider == "" {
		return nil, ErrInvalidProvider
	}
	normalized := config.NormalizeExcludedModels(models)
	if s == nil || s.cfg == nil {
		if len(normalized) == 0 {
			return nil, ErrProviderNotFound
		}
		return map[string][]string{provider: normalized}, nil
	}
	if len(normalized) == 0 {
		if s.cfg.OAuthExcludedModels == nil {
			return nil, ErrProviderNotFound
		}
		if _, ok := s.cfg.OAuthExcludedModels[provider]; !ok {
			return nil, ErrProviderNotFound
		}
		delete(s.cfg.OAuthExcludedModels, provider)
		if len(s.cfg.OAuthExcludedModels) == 0 {
			s.cfg.OAuthExcludedModels = nil
		}
		return s.cfg.OAuthExcludedModels, nil
	}
	if s.cfg.OAuthExcludedModels == nil {
		s.cfg.OAuthExcludedModels = make(map[string][]string)
	}
	s.cfg.OAuthExcludedModels[provider] = normalized
	return s.cfg.OAuthExcludedModels, nil
}

func (s *Service) DeleteExcludedModels(provider string) (map[string][]string, error) {
	provider = normalizeKey(provider)
	if provider == "" {
		return nil, ErrInvalidProvider
	}
	if s == nil || s.cfg == nil || s.cfg.OAuthExcludedModels == nil {
		return nil, ErrProviderNotFound
	}
	if _, ok := s.cfg.OAuthExcludedModels[provider]; !ok {
		return nil, ErrProviderNotFound
	}
	delete(s.cfg.OAuthExcludedModels, provider)
	if len(s.cfg.OAuthExcludedModels) == 0 {
		s.cfg.OAuthExcludedModels = nil
	}
	return s.cfg.OAuthExcludedModels, nil
}

func (s *Service) ModelAlias() map[string][]config.OAuthModelAlias {
	if s == nil || s.cfg == nil {
		return nil
	}
	return NormalizeModelAlias(s.cfg.OAuthModelAlias)
}

func (s *Service) SetModelAlias(entries map[string][]config.OAuthModelAlias) map[string][]config.OAuthModelAlias {
	normalized := NormalizeModelAlias(entries)
	if s == nil || s.cfg == nil {
		return normalized
	}
	s.cfg.OAuthModelAlias = normalized
	return s.cfg.OAuthModelAlias
}

func (s *Service) PatchModelAlias(channel string, aliases []config.OAuthModelAlias) (map[string][]config.OAuthModelAlias, error) {
	channel = normalizeKey(channel)
	if channel == "" {
		return nil, ErrInvalidChannel
	}
	normalizedMap := NormalizeModelAlias(map[string][]config.OAuthModelAlias{channel: aliases})
	normalized := normalizedMap[channel]
	if s == nil || s.cfg == nil {
		if len(normalized) == 0 {
			return nil, ErrChannelNotFound
		}
		return map[string][]config.OAuthModelAlias{channel: normalized}, nil
	}
	if len(normalized) == 0 {
		if s.cfg.OAuthModelAlias == nil {
			return nil, ErrChannelNotFound
		}
		if _, ok := s.cfg.OAuthModelAlias[channel]; !ok {
			return nil, ErrChannelNotFound
		}
		delete(s.cfg.OAuthModelAlias, channel)
		if len(s.cfg.OAuthModelAlias) == 0 {
			s.cfg.OAuthModelAlias = nil
		}
		return s.cfg.OAuthModelAlias, nil
	}
	if s.cfg.OAuthModelAlias == nil {
		s.cfg.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	s.cfg.OAuthModelAlias[channel] = normalized
	return s.cfg.OAuthModelAlias, nil
}

func (s *Service) DeleteModelAlias(channel string) (map[string][]config.OAuthModelAlias, error) {
	channel = normalizeKey(channel)
	if channel == "" {
		return nil, ErrInvalidChannel
	}
	if s == nil || s.cfg == nil || s.cfg.OAuthModelAlias == nil {
		return nil, ErrChannelNotFound
	}
	if _, ok := s.cfg.OAuthModelAlias[channel]; !ok {
		return nil, ErrChannelNotFound
	}
	delete(s.cfg.OAuthModelAlias, channel)
	if len(s.cfg.OAuthModelAlias) == 0 {
		s.cfg.OAuthModelAlias = nil
	}
	return s.cfg.OAuthModelAlias, nil
}

func normalizeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
