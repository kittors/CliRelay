package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
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

const defaultCallbackPort = 1455

type OAuthAuth interface {
	GenerateAuthURL(string, *internalcodex.PKCECodes) (string, error)
	ExchangeCodeForTokens(context.Context, string, *internalcodex.PKCECodes) (*internalcodex.CodexAuthBundle, error)
	CreateTokenStorage(*internalcodex.CodexAuthBundle) *internalcodex.CodexTokenStorage
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
type StartForwarderFunc func(port int, provider, targetBase string) (CallbackForwarder, error)
type StopForwarderFunc func(context.Context, int, CallbackForwarder)
type PKCEFunc func() (*internalcodex.PKCECodes, error)
type StateFunc func() (string, error)

type OAuthLoginOptions struct {
	Config              *config.Config
	Auth                OAuthAuth
	AuthDir             string
	ProxyURL            string
	WebUI               bool
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
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	fmt.Println("Initializing Codex authentication...")

	generatePKCE := opts.GeneratePKCE
	if generatePKCE == nil {
		generatePKCE = internalcodex.GeneratePKCECodes
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
		auth = internalcodex.NewCodexAuthWithProxy(opts.Config, opts.ProxyURL)
	}
	authURL, errAuthURL := auth.GenerateAuthURL(state, pkceCodes)
	if errAuthURL != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrAuthURL, errAuthURL)
	}

	opts.Sessions.register(state, "codex")

	callbackPort := opts.CallbackPort
	if callbackPort == 0 {
		callbackPort = defaultCallbackPort
	}
	forwarder := startCallbackForwarder(opts, callbackPort)

	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}
	callbackWaitTimeout := opts.CallbackWaitTimeout
	if callbackWaitTimeout == 0 {
		callbackWaitTimeout = oauthsession.DefaultTTL
	}

	go func() {
		defer stopForwarder(ctx, opts, callbackPort, forwarder)

		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			authErr := internalcodex.NewAuthenticationError(internalcodex.ErrCallbackTimeout, errors.New("callback waiter unavailable"))
			log.Error(internalcodex.GetUserFriendlyMessage(authErr))
			opts.Sessions.setError(state, "Timeout waiting for OAuth callback")
			return
		}
		resultMap, errWait := waitCallback(authDir, "codex", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			authErr := internalcodex.NewAuthenticationError(internalcodex.ErrCallbackTimeout, errWait)
			log.Error(internalcodex.GetUserFriendlyMessage(authErr))
			opts.Sessions.setError(state, "Timeout waiting for OAuth callback")
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := internalcodex.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(internalcodex.GetUserFriendlyMessage(oauthErr))
			opts.Sessions.setError(state, "Bad Request")
			return
		}
		if resultMap["state"] != state {
			authErr := internalcodex.NewAuthenticationError(internalcodex.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]))
			opts.Sessions.setError(state, "State code error")
			log.Error(internalcodex.GetUserFriendlyMessage(authErr))
			return
		}

		code := resultMap["code"]
		log.Debug("Authorization code received, exchanging for tokens...")
		bundle, errExchange := auth.ExchangeCodeForTokens(ctx, code, pkceCodes)
		if errExchange != nil {
			authErr := internalcodex.NewAuthenticationError(internalcodex.ErrCodeExchangeFailed, errExchange)
			opts.Sessions.setError(state, "Failed to exchange authorization code for tokens")
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
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
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Codex services through this CLI")
		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("codex")
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

func startCallbackForwarder(opts OAuthLoginOptions, callbackPort int) CallbackForwarder {
	if !opts.WebUI {
		return nil
	}
	if opts.CallbackTarget == nil {
		log.Warn("failed to compute codex callback target; continuing with manual callback submission")
		return nil
	}
	targetURL, errTarget := opts.CallbackTarget("/codex/callback")
	if errTarget != nil {
		log.WithError(errTarget).Warn("failed to compute codex callback target; continuing with manual callback submission")
		return nil
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, errStart := start(callbackPort, "codex", targetURL)
	if errStart != nil {
		log.WithError(errStart).Warn("failed to start codex callback forwarder; continuing with manual callback submission")
		return nil
	}
	return forwarder
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

func defaultStartForwarder(port int, provider, targetBase string) (CallbackForwarder, error) {
	return oauthcallback.Start(port, provider, targetBase)
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
