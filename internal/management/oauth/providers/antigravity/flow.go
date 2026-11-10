package antigravity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	oauthcallback "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/callback"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	ErrStateGeneration      = errors.New("failed to generate state parameter")
	ErrCallbackUnavailable  = errors.New("callback server unavailable")
	ErrCallbackStart        = errors.New("failed to start callback server")
	ErrOAuthClientIDMissing = errors.New("antigravity oauth client-id not configured")
)

type OAuthAuth interface {
	BuildAuthURL(state, redirectURI string) string
	ExchangeCodeForTokens(context.Context, string, string) (*internalantigravity.TokenResponse, error)
	FetchUserInfo(context.Context, string) (string, error)
	FetchProjectID(context.Context, string) (string, error)
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
	GenerateState       StateFunc
	Now                 func() time.Time
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	fmt.Println("Initializing Antigravity authentication...")

	auth := opts.Auth
	if auth == nil {
		auth = internalantigravity.NewAntigravityAuthWithProxy(opts.Config, opts.ProxyURL)
	}

	generateState := opts.GenerateState
	if generateState == nil {
		generateState = misc.GenerateRandomState
	}
	state, errState := generateState()
	if errState != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrStateGeneration, errState)
	}

	callbackPort := opts.CallbackPort
	if callbackPort == 0 {
		callbackPort = internalantigravity.CallbackPort
	}
	forwarder, actualPort, errForwarder := startCallbackForwarder(opts, callbackPort)
	if errForwarder != nil {
		return OAuthLoginResult{}, errForwarder
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", actualPort)
	authURL := auth.BuildAuthURL(state, redirectURI)
	if strings.TrimSpace(authURL) == "" {
		stopForwarder(ctx, opts, actualPort, forwarder)
		return OAuthLoginResult{}, ErrOAuthClientIDMissing
	}

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

	opts.Sessions.register(state, "antigravity")

	go func() {
		defer stopForwarder(ctx, opts, actualPort, forwarder)

		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			log.Error("oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		payload, errWait := waitCallback(authDir, "antigravity", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			log.Error("oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
			log.Errorf("Authentication failed: %s", errStr)
			opts.Sessions.setError(state, "Authentication failed")
			return
		}
		if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
			log.Errorf("Authentication failed: state mismatch")
			opts.Sessions.setError(state, "Authentication failed: state mismatch")
			return
		}
		authCode := strings.TrimSpace(payload["code"])
		if authCode == "" {
			log.Error("Authentication failed: code not found")
			opts.Sessions.setError(state, "Authentication failed: code not found")
			return
		}

		tokenResp, errToken := auth.ExchangeCodeForTokens(ctx, authCode, redirectURI)
		if errToken != nil {
			log.Errorf("Failed to exchange token: %v", errToken)
			opts.Sessions.setError(state, "Failed to exchange token")
			return
		}
		accessToken := ""
		if tokenResp != nil {
			accessToken = strings.TrimSpace(tokenResp.AccessToken)
		}
		if accessToken == "" {
			log.Error("antigravity: token exchange returned empty access token")
			opts.Sessions.setError(state, "Failed to exchange token")
			return
		}

		email, errInfo := auth.FetchUserInfo(ctx, accessToken)
		if errInfo != nil {
			log.Errorf("Failed to fetch user info: %v", errInfo)
			opts.Sessions.setError(state, "Failed to fetch user info")
			return
		}
		email = strings.TrimSpace(email)
		if email == "" {
			log.Error("antigravity: user info returned empty email")
			opts.Sessions.setError(state, "Failed to fetch user info")
			return
		}

		// An account is only usable with its Cloud Code project. Saving it
		// without one and reporting success is how #1032's accounts came to
		// send made-up projects and draw 403 SUBSCRIPTION_REQUIRED, so the login
		// fails here and says why; the operator can retry once Google has
		// finished onboarding the account.
		projectID, errProject := auth.FetchProjectID(ctx, accessToken)
		projectID = strings.TrimSpace(projectID)
		if errProject == nil && projectID == "" {
			errProject = internalantigravity.ErrNoProjectID
		}
		if errProject != nil {
			log.Errorf("antigravity: project discovery failed: %v", errProject)
			opts.Sessions.setError(state, ProjectDiscoveryFailureMessage(errProject))
			return
		}
		log.Infof("antigravity: obtained project ID %s", projectID)

		record := RecordFromTokenResponse(tokenResp, email, projectID, now())
		if opts.SaveRecord == nil {
			opts.Sessions.setError(state, "Failed to save token to file")
			return
		}
		savedPath, errSave := opts.SaveRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			opts.Sessions.setError(state, "Failed to save token to file")
			return
		}

		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("antigravity")
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Printf("Using GCP project: %s\n", projectID)
		fmt.Println("You can now use Antigravity services through this CLI")
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

// ProjectDiscoveryFailureMessage is what a login reports when the account's
// Cloud Code project could not be found. It keeps Google's reason, since "no
// project" on its own reads like a missing licence, and says when trying again
// can help.
func ProjectDiscoveryFailureMessage(err error) string {
	reason := "unknown error"
	if err != nil {
		reason = strings.Join(strings.Fields(err.Error()), " ")
		if len(reason) > 300 {
			reason = reason[:300] + "…"
		}
	}
	switch {
	case errors.Is(err, internalantigravity.ErrUnsupportedClient):
		return "Google rejected this client while looking up the Antigravity project: " + reason
	case errors.Is(err, internalantigravity.ErrOnboardingPending):
		return "Google has not finished setting up this account for Antigravity yet; log in again shortly: " + reason
	case errors.Is(err, internalantigravity.ErrNoProjectID):
		return "Google returned no Antigravity project for this account: " + reason
	default:
		return "Failed to look up the Antigravity project: " + reason
	}
}

// projectDiscoveryError carries ProjectDiscoveryFailureMessage while keeping
// the cause reachable, so callers can still tell a timeout from a verdict.
type projectDiscoveryError struct{ cause error }

func (e projectDiscoveryError) Error() string { return ProjectDiscoveryFailureMessage(e.cause) }
func (e projectDiscoveryError) Unwrap() error { return e.cause }

func startCallbackForwarder(opts OAuthLoginOptions, callbackPort int) (CallbackForwarder, int, error) {
	if !opts.WebUI {
		return nil, callbackPort, nil
	}
	if opts.CallbackTarget == nil {
		return nil, callbackPort, ErrCallbackUnavailable
	}
	targetURL, errTarget := opts.CallbackTarget("/antigravity/callback")
	if errTarget != nil {
		return nil, callbackPort, fmt.Errorf("%w: %v", ErrCallbackUnavailable, errTarget)
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, actualPort, errStart := start(callbackPort, "antigravity", targetURL)
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
