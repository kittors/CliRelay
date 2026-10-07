package xai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// SSOConverter is the part of XAIAuth an SSO import needs; tests fake it.
type SSOConverter interface {
	ConvertSSOToTokens(ctx context.Context, ssoToken string) (*internalxai.TokenData, string, error)
	CreateTokenStorage(bundle *internalxai.AuthBundle) *internalxai.TokenStorage
}

// SSOImportOptions configures ImportSSO.
type SSOImportOptions struct {
	Config *config.Config
	// ProxyURL is the egress x.ai is reached through — the same one the
	// account will use afterwards.
	ProxyURL string
	SSOToken string
	// UsingAPI selects api.x.ai (API credit) over the Grok Build endpoint, as
	// in the browser login.
	UsingAPI  bool
	Converter SSOConverter
	Now       func() time.Time
}

// ImportSSO turns a Grok web session cookie into a Grok Build account.
func ImportSSO(ctx context.Context, opts SSOImportOptions) (*coreauth.Auth, credentialimport.Details, error) {
	converter := opts.Converter
	if converter == nil {
		converter = internalxai.NewXAIAuthWithProxyURL(opts.Config, opts.ProxyURL)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	tokenData, tokenEndpoint, err := converter.ConvertSSOToTokens(ctx, opts.SSOToken)
	if err != nil {
		return nil, credentialimport.Details{}, classifySSOError(err)
	}
	if tokenData == nil {
		return nil, credentialimport.Details{}, errors.New("xai returned no tokens")
	}
	bundle := &internalxai.AuthBundle{
		TokenData:     *tokenData,
		LastRefresh:   now().UTC().Format(time.RFC3339),
		BaseURL:       internalxai.DefaultAPIBaseURL,
		TokenEndpoint: tokenEndpoint,
	}
	record := RecordFromTokenStorage(converter.CreateTokenStorage(bundle), now(), opts.UsingAPI)
	if record == nil {
		return nil, credentialimport.Details{}, errors.New("xai returned no access token")
	}
	return record, credentialimport.Details{Email: strings.TrimSpace(tokenData.Email)}, nil
}

func classifySSOError(err error) error {
	switch {
	case errors.Is(err, internalxai.ErrSSOInvalid):
		return fmt.Errorf("%w: %v", credentialimport.ErrInvalidCredential, err)
	case errors.Is(err, internalxai.ErrSSODenied):
		return fmt.Errorf("%w: %v", credentialimport.ErrDenied, err)
	}
	return err
}
