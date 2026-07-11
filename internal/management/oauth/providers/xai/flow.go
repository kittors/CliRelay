package xai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	oauthcallback "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/callback"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	ErrPKCEGeneration      = errors.New("failed to generate PKCE codes")
	ErrStateGeneration     = errors.New("failed to generate state parameter")
	ErrDiscovery           = errors.New("failed to discover xai oauth endpoints")
	ErrAuthURL             = errors.New("failed to generate authorization url")
	ErrCallbackUnavailable = errors.New("callback server unavailable")
	ErrCallbackStart       = errors.New("failed to start callback server")
)

type OAuthAuth interface {
	Discover(context.Context) (*internalxai.Discovery, error)
	ExchangeCodeForTokens(context.Context, string, string, *internalxai.PKCECodes, string) (*internalxai.AuthBundle, error)
	CreateTokenStorage(*internalxai.AuthBundle) *internalxai.TokenStorage
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
type PKCEFunc func() (*internalxai.PKCECodes, error)
type StateFunc func() (string, error)

type OAuthLoginOptions struct {
	Config              *config.Config
	Auth                OAuthAuth
	AuthDir             string
	WebUI               bool
	UsingAPI            bool
	CallbackPort        int
	CallbackWaitTimeout time.Duration
	CallbackTarget      CallbackTargetFunc
	StartForwarder      StartForwarderFunc
	StopForwarder       StopForwarderFunc
	WaitCallback        WaitCallbackFunc
	Sessions            SessionCallbacks
	SaveRecord          SaveRecordFunc
	GeneratePKCE        PKCEFunc
	GenerateState       StateFunc
	GenerateNonce       StateFunc
	Now                 func() time.Time
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	fmt.Println("Initializing xAI authentication...")

	generatePKCE := opts.GeneratePKCE
	if generatePKCE == nil {
		generatePKCE = internalxai.GeneratePKCECodes
	}
	pkceCodes, errPKCE := generatePKCE()
	if errPKCE != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrPKCEGeneration, errPKCE)
	}

	state, errState := generateOAuthValue(opts.GenerateState)
	if errState != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrStateGeneration, errState)
	}
	nonce, errNonce := generateOAuthValue(opts.GenerateNonce)
	if errNonce != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrStateGeneration, errNonce)
	}

	auth := opts.Auth
	if auth == nil {
		auth = internalxai.NewXAIAuth(opts.Config)
	}
	discovery, errDiscovery := auth.Discover(ctx)
	if errDiscovery != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrDiscovery, errDiscovery)
	}

	callbackPort := opts.CallbackPort
	if callbackPort == 0 {
		callbackPort = internalxai.CallbackPort
	}
	forwarder, actualPort, errForwarder := startCallbackForwarder(opts, callbackPort)
	if errForwarder != nil {
		return OAuthLoginResult{}, errForwarder
	}

	redirectURI := fmt.Sprintf("http://%s:%d%s", internalxai.RedirectHost, actualPort, internalxai.RedirectPath)
	authURL, errAuthURL := internalxai.BuildAuthorizeURL(internalxai.AuthorizeURLParams{
		AuthorizationEndpoint: discovery.AuthorizationEndpoint,
		RedirectURI:           redirectURI,
		CodeChallenge:         pkceCodes.CodeChallenge,
		State:                 state,
		Nonce:                 nonce,
	})
	if errAuthURL != nil {
		stopForwarder(ctx, opts, actualPort, forwarder)
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrAuthURL, errAuthURL)
	}

	opts.Sessions.register(state, "xai")

	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}
	callbackWaitTimeout := opts.CallbackWaitTimeout
	if callbackWaitTimeout == 0 {
		callbackWaitTimeout = oauthsession.DefaultTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	go func() {
		defer stopForwarder(ctx, opts, actualPort, forwarder)

		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			log.Error("xai oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		payload, errWait := waitCallback(authDir, "xai", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			log.Error("xai oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
			log.Errorf("xai authentication failed: %s", errStr)
			opts.Sessions.setError(state, "Authentication failed")
			return
		}
		if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
			log.Error("xai authentication failed: state mismatch")
			opts.Sessions.setError(state, "Authentication failed: state mismatch")
			return
		}
		authCode := strings.TrimSpace(payload["code"])
		if authCode == "" {
			log.Error("xai authentication failed: code not found")
			opts.Sessions.setError(state, "Authentication failed: code not found")
			return
		}

		bundle, errExchange := auth.ExchangeCodeForTokens(ctx, authCode, redirectURI, pkceCodes, discovery.TokenEndpoint)
		if errExchange != nil {
			log.Errorf("xai token exchange failed: %v", errExchange)
			opts.Sessions.setError(state, "Failed to exchange token")
			return
		}
		tokenStorage := auth.CreateTokenStorage(bundle)
		record := RecordFromTokenStorage(tokenStorage, now(), opts.UsingAPI)
		if record == nil {
			log.Error("xai token exchange returned empty access token")
			opts.Sessions.setError(state, "Failed to exchange token")
			return
		}
		if opts.SaveRecord == nil {
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			return
		}
		savedPath, errSave := opts.SaveRecord(ctx, record)
		if errSave != nil {
			log.Errorf("failed to save xai token: %v", errSave)
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use xAI services through this CLI")
		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("xai")
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

func generateOAuthValue(fn StateFunc) (string, error) {
	if fn == nil {
		fn = misc.GenerateRandomState
	}
	value, err := fn()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func startCallbackForwarder(opts OAuthLoginOptions, callbackPort int) (CallbackForwarder, int, error) {
	if !opts.WebUI {
		return nil, callbackPort, nil
	}
	if opts.CallbackTarget == nil {
		return nil, callbackPort, ErrCallbackUnavailable
	}
	targetURL, errTarget := opts.CallbackTarget("/xai/callback")
	if errTarget != nil {
		return nil, callbackPort, fmt.Errorf("%w: %v", ErrCallbackUnavailable, errTarget)
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, actualPort, errStart := start(callbackPort, "xai", targetURL)
	if errStart != nil {
		return nil, callbackPort, fmt.Errorf("%w: %v", ErrCallbackStart, errStart)
	}
	return forwarder, actualPort, nil
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
