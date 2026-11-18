package providers

import (
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var ErrItemNotFound = errors.New("item not found")

type Validator func() error

type Service struct {
	cfg      *config.Config
	validate Validator
}

func NewService(cfg *config.Config, validate Validator) *Service {
	return &Service{cfg: cfg, validate: validate}
}

func (s *Service) runValidator() error {
	if s == nil || s.validate == nil {
		return nil
	}
	return s.validate()
}

func prepareProviderStableIDs(previous, next *config.Config) {
	config.PreserveMissingProviderStableIDs(previous, next)
	next.EnsureProviderStableIDs()
}
