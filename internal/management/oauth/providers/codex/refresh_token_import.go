package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// RefreshTokenRefresher is the part of CodexAuth an import needs; tests fake it.
type RefreshTokenRefresher interface {
	RefreshTokens(ctx context.Context, refreshToken string) (*internalcodex.CodexTokenData, error)
}

// RefreshTokenImportOptions configures ImportRefreshToken.
type RefreshTokenImportOptions struct {
	Config *config.Config
	// ProxyURL is the egress the refresh leaves from — the same one the account
	// will use afterwards.
	ProxyURL     string
	RefreshToken string
	Refresher    RefreshTokenRefresher
	Now          func() time.Time
}

// ImportRefreshToken turns a Codex CLI refresh token into an account.
//
// OpenAI refresh tokens are single-use: refreshing returns a new one and
// retires the old, so the import takes the credential over and whatever held
// it before (a local Codex CLI, another relay) is signed out. The rotated token
// is what gets stored.
func ImportRefreshToken(ctx context.Context, opts RefreshTokenImportOptions) (*coreauth.Auth, credentialimport.Details, error) {
	refreshToken := strings.TrimSpace(opts.RefreshToken)
	if refreshToken == "" {
		return nil, credentialimport.Details{}, credentialimport.ErrInvalidCredential
	}
	refresher := opts.Refresher
	if refresher == nil {
		refresher = internalcodex.NewCodexAuthWithProxy(opts.Config, opts.ProxyURL)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	tokenData, err := refresher.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, credentialimport.Details{}, classifyRefreshError(err)
	}
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return nil, credentialimport.Details{}, errors.New("openai returned no access token")
	}
	rotated := strings.TrimSpace(tokenData.RefreshToken)
	if rotated == "" {
		// Defensive: OpenAI rotates on every refresh, but keep the one that
		// just worked rather than store an account that cannot refresh.
		rotated = refreshToken
	}

	storage := &internalcodex.CodexTokenStorage{
		IDToken:      tokenData.IDToken,
		AccessToken:  tokenData.AccessToken,
		RefreshToken: rotated,
		AccountID:    tokenData.AccountID,
		LastRefresh:  now().Format(time.RFC3339),
		Email:        tokenData.Email,
		Expire:       tokenData.Expire,
	}
	record := RecordFromTokenStorage(storage)
	if record == nil {
		return nil, credentialimport.Details{}, errors.New("could not build the codex account")
	}
	planType, _ := planAndAccountHashFromIDToken(tokenData.IDToken)
	return record, credentialimport.Details{Email: strings.TrimSpace(tokenData.Email), Plan: planType}, nil
}

// classifyRefreshError sorts a refresh failure into the import's error kinds.
// CodexAuth reports upstream failures as text carrying the status and body.
func classifyRefreshError(err error) error {
	raw := strings.ToLower(err.Error())
	switch {
	case strings.Contains(raw, "status 403") &&
		(strings.Contains(raw, "just a moment") || strings.Contains(raw, "cf-chl") || strings.Contains(raw, "challenge-platform")):
		return fmt.Errorf("%w: %v", credentialimport.ErrBlocked, err)
	case strings.Contains(raw, "invalid_grant"),
		strings.Contains(raw, "refresh_token_reused"),
		strings.Contains(raw, "token has been revoked"),
		strings.Contains(raw, "token has been expired"),
		strings.Contains(raw, "invalid_request") && strings.Contains(raw, "refresh"),
		strings.Contains(raw, "status 401"):
		return fmt.Errorf("%w: %v", credentialimport.ErrInvalidCredential, err)
	}
	return err
}
