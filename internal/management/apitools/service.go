package apitools

import (
	"context"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ClaudeOAuthRefresher interface {
	RefreshTokens(ctx context.Context, refreshToken string) (*claudeauth.ClaudeTokenData, error)
}

type Dependencies struct {
	DefaultAPICallTimeout             time.Duration
	ManagementAPICallResponseLimit    int64
	ManagementOAuthTokenResponseLimit int64
	GeminiOAuthScopes                 []string
	AntigravityOAuthTokenURL          string
	NewClaudeOAuthRefresher           func(*config.Config) ClaudeOAuthRefresher
	KimiOAuthClientID                 string
	KimiOAuthTokenURL                 string
}

type Service struct {
	tenantID    string
	cfg         *config.Config
	authManager *coreauth.Manager
	deps        Dependencies
}

func New(cfg *config.Config, authManager *coreauth.Manager, deps Dependencies) *Service {
	return NewForTenant("", cfg, authManager, deps)
}

func NewForTenant(tenantID string, cfg *config.Config, authManager *coreauth.Manager, deps Dependencies) *Service {
	return &Service{tenantID: coreauth.NormalizedTenantID(tenantID), cfg: cfg, authManager: authManager, deps: deps}
}

func (s *Service) defaultAPICallTimeout() time.Duration {
	if s == nil || s.deps.DefaultAPICallTimeout <= 0 {
		return 60 * time.Second
	}
	return s.deps.DefaultAPICallTimeout
}

func (s *Service) apiCallResponseLimit() int64 {
	if s == nil || s.deps.ManagementAPICallResponseLimit <= 0 {
		return 4 << 20
	}
	return s.deps.ManagementAPICallResponseLimit
}

func (s *Service) oauthTokenResponseLimit() int64 {
	if s == nil || s.deps.ManagementOAuthTokenResponseLimit <= 0 {
		return 64 << 10
	}
	return s.deps.ManagementOAuthTokenResponseLimit
}
