package claude

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	oauthcallback "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/callback"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	ErrPKCEGeneration  = errors.New("failed to generate PKCE codes")
	ErrStateGeneration = errors.New("failed to generate state parameter")
	ErrAuthURL         = errors.New("failed to generate authorization url")
)

const defaultCallbackPort = 54545

type OAuthAuth interface {
	GenerateAuthURLWithRedirectURI(string, *internalclaude.PKCECodes, string) (string, string, error)
	ExchangeCodeForTokensWithRedirectURI(context.Context, string, string, *internalclaude.PKCECodes, string) (*internalclaude.ClaudeAuthBundle, error)
	CreateTokenStorage(*internalclaude.ClaudeAuthBundle) *internalclaude.ClaudeTokenStorage
}

type SessionCallbacks struct {
	Register         func(state, provider string)
	SetError         func(state, message string)
	Complete         func(state string)
	CompleteProvider func(provider string) int
}

type SaveRecordFunc func(context.Context, *coreauth.Auth) (string, error)
type WaitCallbackFunc func(authDir, provider, state string, timeout time.Duration) (map[string]string, error)
type CallbackTargetFunc func(path string) (string, error)
type CallbackForwarder any
type StartForwarderFunc func(preferredPort int, provider, targetBase string) (CallbackForwarder, int, error)
type StopForwarderFunc func(context.Context, int, CallbackForwarder)
type PKCEFunc func() (*internalclaude.PKCECodes, error)
type StateFunc func() (string, error)

type OAuthLoginOptions struct {
	Config                *config.Config
	Auth                  OAuthAuth
	AuthDir               string
	ProxyURL              string
	WebUI                 bool
	PreferredCallbackPort int
	CallbackWaitTimeout   time.Duration
	CallbackTarget        CallbackTargetFunc
	StartForwarder        StartForwarderFunc
	StopForwarder         StopForwarderFunc
	WaitCallback          WaitCallbackFunc
	Sessions              SessionCallbacks
	SaveRecord            SaveRecordFunc
	GeneratePKCE          PKCEFunc
	GenerateState         StateFunc
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	fmt.Println("Initializing Claude authentication...")

	generatePKCE := opts.GeneratePKCE
	if generatePKCE == nil {
		generatePKCE = internalclaude.GeneratePKCECodes
	}
	pkceCodes, errPKCE := generatePKCE()
	if errPKCE != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrPKCEGeneration, errPKCE)
	}

	generateState := opts.GenerateState
	if generateState == nil {
		generateState = misc.GenerateRandomState
	}
	state, errState := generateState()
	if errState != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrStateGeneration, errState)
	}

	auth := opts.Auth
	if auth == nil {
		auth = internalclaude.NewClaudeAuthWithProxy(opts.Config, opts.ProxyURL)
	}

	redirectURI, forwarder, callbackPort := resolveRedirectURI(opts)
	authURL, state, errAuthURL := auth.GenerateAuthURLWithRedirectURI(state, pkceCodes, redirectURI)
	if errAuthURL != nil {
		stopForwarder(ctx, opts, callbackPort, forwarder)
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrAuthURL, errAuthURL)
	}

	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}
	callbackWaitTimeout := opts.CallbackWaitTimeout
	if callbackWaitTimeout == 0 {
		callbackWaitTimeout = oauthsession.DefaultTTL
	}

	opts.Sessions.register(state, "anthropic")

	go func() {
		defer stopForwarder(ctx, opts, callbackPort, forwarder)

		fmt.Println("Waiting for authentication callback...")
		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			opts.Sessions.setError(state, "Timeout waiting for OAuth callback")
			log.Error(internalclaude.GetUserFriendlyMessage(internalclaude.NewAuthenticationError(internalclaude.ErrCallbackTimeout, errors.New("callback waiter unavailable"))))
			return
		}
		resultMap, errWait := waitCallback(authDir, "anthropic", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			opts.Sessions.setError(state, "Timeout waiting for OAuth callback")
			authErr := internalclaude.NewAuthenticationError(internalclaude.ErrCallbackTimeout, errWait)
			log.Error(internalclaude.GetUserFriendlyMessage(authErr))
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := internalclaude.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(internalclaude.GetUserFriendlyMessage(oauthErr))
			opts.Sessions.setError(state, "Bad request")
			return
		}
		if resultMap["state"] != state {
			authErr := internalclaude.NewAuthenticationError(internalclaude.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]))
			log.Error(internalclaude.GetUserFriendlyMessage(authErr))
			opts.Sessions.setError(state, "State code error")
			return
		}

		code := strings.Split(resultMap["code"], "#")[0]
		bundle, errExchange := auth.ExchangeCodeForTokensWithRedirectURI(ctx, code, state, pkceCodes, redirectURI)
		if errExchange != nil {
			authErr := internalclaude.NewAuthenticationError(internalclaude.ErrCodeExchangeFailed, errExchange)
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			opts.Sessions.setError(state, "Failed to exchange authorization code for tokens")
			return
		}

		tokenStorage := auth.CreateTokenStorage(bundle)
		record := RecordFromTokenStorage(tokenStorage)
		if opts.SaveRecord == nil {
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			return
		}
		savedPath, errSave := opts.SaveRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Claude services through this CLI")
		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("anthropic")
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

func resolveRedirectURI(opts OAuthLoginOptions) (string, CallbackForwarder, int) {
	redirectURI := internalclaude.RedirectURI
	callbackPort := opts.PreferredCallbackPort
	if callbackPort == 0 {
		callbackPort = defaultCallbackPort
	}
	if !opts.WebUI {
		return redirectURI, nil, callbackPort
	}
	if opts.CallbackTarget == nil {
		log.Warn("failed to compute anthropic callback target, falling back to Claude platform callback")
		return internalclaude.PlatformRedirectURI, nil, callbackPort
	}
	targetURL, errTarget := opts.CallbackTarget("/anthropic/callback")
	if errTarget != nil {
		log.WithError(errTarget).Warn("failed to compute anthropic callback target, falling back to Claude platform callback")
		return internalclaude.PlatformRedirectURI, nil, callbackPort
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, actualPort, errStart := start(callbackPort, "anthropic", targetURL)
	if errStart != nil {
		log.WithError(errStart).Warn("failed to start anthropic callback forwarder, falling back to Claude platform callback")
		return internalclaude.PlatformRedirectURI, nil, callbackPort
	}
	return fmt.Sprintf("http://localhost:%d/callback", actualPort), forwarder, actualPort
}

func stopForwarder(ctx context.Context, opts OAuthLoginOptions, port int, forwarder CallbackForwarder) {
	if forwarder == nil {
		return
	}
	stop := opts.StopForwarder
	if stop == nil {
		stop = defaultStopForwarder
	}
	stop(ctx, port, forwarder)
}

func defaultStartForwarder(preferredPort int, provider, targetBase string) (CallbackForwarder, int, error) {
	return oauthcallback.StartOnAvailablePort(preferredPort, provider, targetBase)
}

func defaultStopForwarder(ctx context.Context, port int, forwarder CallbackForwarder) {
	instance, _ := forwarder.(*oauthcallback.Forwarder)
	oauthcallback.StopInstance(ctx, port, instance)
}

func (s SessionCallbacks) register(state, provider string) {
	if s.Register != nil {
		s.Register(state, provider)
	}
}

func (s SessionCallbacks) setError(state, message string) {
	if s.SetError != nil {
		s.SetError(state, message)
	}
}

func (s SessionCallbacks) complete(state string) {
	if s.Complete != nil {
		s.Complete(state)
	}
}

func (s SessionCallbacks) completeProvider(provider string) int {
	if s.CompleteProvider == nil {
		return 0
	}
	return s.CompleteProvider(provider)
}
