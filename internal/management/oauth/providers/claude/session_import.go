package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// SessionAuthorizer is the part of ClaudeAuth a session import needs; tests
// fake it.
type SessionAuthorizer interface {
	AuthorizeWithSessionKey(ctx context.Context, sessionKey string) (*internalclaude.ClaudeAuthBundle, internalclaude.SessionOrganization, error)
	CreateTokenStorage(bundle *internalclaude.ClaudeAuthBundle) *internalclaude.ClaudeTokenStorage
}

// SessionImportOptions configures ImportSessionKey.
type SessionImportOptions struct {
	Config *config.Config
	// ProxyURL is the egress claude.ai is reached through — the same one the
	// account will use afterwards.
	ProxyURL   string
	SessionKey string
	Authorizer SessionAuthorizer
}

// ImportSessionKey turns a claude.ai sessionKey cookie into an account, with
// the same tokens and file a browser login would have produced.
func ImportSessionKey(ctx context.Context, opts SessionImportOptions) (*coreauth.Auth, credentialimport.Details, error) {
	authorizer := opts.Authorizer
	if authorizer == nil {
		authorizer = internalclaude.NewClaudeAuthWithProxy(opts.Config, opts.ProxyURL)
	}
	bundle, org, err := authorizer.AuthorizeWithSessionKey(ctx, opts.SessionKey)
	if err != nil {
		return nil, credentialimport.Details{}, classifySessionError(err)
	}
	record := RecordFromTokenStorage(authorizer.CreateTokenStorage(bundle))
	if record == nil {
		return nil, credentialimport.Details{}, errors.New("could not build the claude account")
	}
	return record, credentialimport.Details{
		Email:        strings.TrimSpace(bundle.TokenData.Email),
		Organization: org.Name,
	}, nil
}

func classifySessionError(err error) error {
	switch {
	case errors.Is(err, internalclaude.ErrSessionKeyInvalid):
		return fmt.Errorf("%w: %v", credentialimport.ErrInvalidCredential, err)
	case errors.Is(err, internalclaude.ErrSessionBlocked):
		return fmt.Errorf("%w: %v", credentialimport.ErrBlocked, err)
	case errors.Is(err, internalclaude.ErrNoOrganization):
		return fmt.Errorf("%w: %v", credentialimport.ErrNoOrganization, err)
	}
	return err
}
