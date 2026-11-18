package iflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internaliflow "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	oauthcallback "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/callback"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	ErrCallbackUnavailable = errors.New("callback server unavailable")
	ErrCallbackStart       = errors.New("failed to start callback server")
)

type OAuthAuth interface {
	AuthorizationURL(state string, port int) (authURL, redirectURI string)
	ExchangeCodeForTokens(context.Context, string, string) (*internaliflow.IFlowTokenData, error)
	CreateTokenStorage(*internaliflow.IFlowTokenData) *internaliflow.IFlowTokenStorage
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

type OAuthLoginOptions struct {
	Config              *config.Config
	Auth                OAuthAuth
	AuthDir             string
	WebUI               bool
	CallbackPort        int
	CallbackWaitTimeout time.Duration
	CallbackTarget      CallbackTargetFunc
	StartForwarder      StartForwarderFunc
	StopForwarder       StopForwarderFunc
	WaitCallback        WaitCallbackFunc
	Sessions            SessionCallbacks
	SaveRecord          SaveRecordFunc
	Now                 func() time.Time
	State               func() string
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	fmt.Println("Initializing iFlow authentication...")

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	state := ""
	if opts.State != nil {
		state = opts.State()
	}
	if state == "" {
		state = fmt.Sprintf("ifl-%d", now().UnixNano())
	}
	auth := opts.Auth
	if auth == nil {
		auth = internaliflow.NewIFlowAuth(opts.Config)
	}
	callbackPort := opts.CallbackPort
	if callbackPort == 0 {
		callbackPort = internaliflow.CallbackPort
	}
	callbackWaitTimeout := opts.CallbackWaitTimeout
	if callbackWaitTimeout == 0 {
		callbackWaitTimeout = oauthsession.DefaultTTL
	}
	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}

	authURL, redirectURI := auth.AuthorizationURL(state, callbackPort)
	opts.Sessions.register(state, "iflow")

	forwarder, errForwarder := startCallbackForwarder(opts, callbackPort)
	if errForwarder != nil {
		return OAuthLoginResult{}, errForwarder
	}

	go func() {
		if opts.WebUI {
			stop := opts.StopForwarder
			if stop == nil {
				stop = defaultStopForwarder
			}
			defer stop(ctx, callbackPort, forwarder)
		}
		fmt.Println("Waiting for authentication...")

		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Println("Authentication failed: callback waiter unavailable")
			return
		}
		resultMap, errWait := waitCallback(authDir, "iflow", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Println("Authentication failed: timeout waiting for callback")
			return
		}

		if errStr := strings.TrimSpace(resultMap["error"]); errStr != "" {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %s\n", errStr)
			return
		}
		if resultState := strings.TrimSpace(resultMap["state"]); resultState != state {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Println("Authentication failed: state mismatch")
			return
		}

		code := strings.TrimSpace(resultMap["code"])
		if code == "" {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Println("Authentication failed: code missing")
			return
		}

		tokenData, errExchange := auth.ExchangeCodeForTokens(ctx, code, redirectURI)
		if errExchange != nil {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errExchange)
			return
		}

		tokenStorage := auth.CreateTokenStorage(tokenData)
		record := RecordFromTokenStorage(tokenStorage, now())
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
		if tokenStorage.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use iFlow services through this CLI")
		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("iflow")
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

func startCallbackForwarder(opts OAuthLoginOptions, callbackPort int) (CallbackForwarder, error) {
	if !opts.WebUI {
		return nil, nil
	}
	if opts.CallbackTarget == nil {
		return nil, ErrCallbackUnavailable
	}
	targetURL, errTarget := opts.CallbackTarget("/iflow/callback")
	if errTarget != nil {
		return nil, fmt.Errorf("%w: %v", ErrCallbackUnavailable, errTarget)
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, errStart := start(callbackPort, "iflow", targetURL)
	if errStart != nil {
		return nil, fmt.Errorf("%w: %v", ErrCallbackStart, errStart)
	}
	return forwarder, nil
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
