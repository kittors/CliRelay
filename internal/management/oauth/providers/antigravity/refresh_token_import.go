package antigravity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RefreshTokenClient is the part of AntigravityAuth an import needs; tests
// fake it.
type RefreshTokenClient interface {
	RefreshAccessToken(ctx context.Context, refreshToken string) (*internalantigravity.TokenResponse, error)
	FetchUserInfo(ctx context.Context, accessToken string) (string, error)
	FetchProjectID(ctx context.Context, accessToken string) (string, error)
}

// RefreshTokenImportOptions configures ImportRefreshToken.
type RefreshTokenImportOptions struct {
	Config *config.Config
	// ProxyURL is the egress Google is reached through — the same one the
	// account will use afterwards.
	ProxyURL     string
	RefreshToken string
	Client       RefreshTokenClient
	Now          func() time.Time
}

// ImportRefreshToken turns an Antigravity (Google) refresh token into an
// account: refresh, read the email, look up the CloudCode project — the same
// steps the browser login takes after its code exchange.
func ImportRefreshToken(ctx context.Context, opts RefreshTokenImportOptions) (*coreauth.Auth, credentialimport.Details, error) {
	refreshToken := strings.TrimSpace(opts.RefreshToken)
	if refreshToken == "" {
		return nil, credentialimport.Details{}, credentialimport.ErrInvalidCredential
	}
	client := opts.Client
	if client == nil {
		client = internalantigravity.NewAntigravityAuthWithProxy(opts.Config, opts.ProxyURL)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	tokenResp, err := client.RefreshAccessToken(ctx, refreshToken)
	if err != nil {
		return nil, credentialimport.Details{}, classifyRefreshError(err)
	}
	accessToken := ""
	if tokenResp != nil {
		accessToken = strings.TrimSpace(tokenResp.AccessToken)
	}
	if accessToken == "" {
		return nil, credentialimport.Details{}, errors.New("google returned no access token")
	}
	if strings.TrimSpace(tokenResp.RefreshToken) == "" {
		// Google keeps refresh tokens stable and usually omits them here.
		tokenResp.RefreshToken = refreshToken
	}

	email, err := client.FetchUserInfo(ctx, accessToken)
	if err != nil {
		return nil, credentialimport.Details{}, fmt.Errorf("read the google account: %w", err)
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, credentialimport.Details{}, errors.New("google returned no email for this account")
	}
	// Same rule as the browser login: an account is only usable with its
	// Cloud Code project, so an import that cannot find one fails and says why
	// rather than saving an account that cannot serve (#1032).
	projectID, errProject := client.FetchProjectID(ctx, accessToken)
	projectID = strings.TrimSpace(projectID)
	if errProject == nil && projectID == "" {
		errProject = internalantigravity.ErrNoProjectID
	}
	if errProject != nil {
		log.Warnf("antigravity import: project discovery failed: %v", errProject)
		return nil, credentialimport.Details{}, projectDiscoveryError{cause: errProject}
	}

	record := RecordFromTokenResponse(tokenResp, email, projectID, now())
	if record == nil {
		return nil, credentialimport.Details{}, errors.New("could not build the antigravity account")
	}
	return record, credentialimport.Details{Email: email}, nil
}

// classifyRefreshError sorts Google's refusals: invalid_grant is a revoked or
// expired token, unauthorized_client a token issued to some other client.
func classifyRefreshError(err error) error {
	raw := strings.ToLower(err.Error())
	if strings.Contains(raw, "invalid_grant") || strings.Contains(raw, "unauthorized_client") ||
		strings.Contains(raw, "status 401") {
		return fmt.Errorf("%w: %v", credentialimport.ErrInvalidCredential, err)
	}
	return err
}
