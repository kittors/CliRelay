package geminicli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	geminiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	oauthcallback "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/callback"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	ErrOAuthClientIDMissing = errors.New("gemini oauth client-id not configured")
	ErrStateGeneration      = errors.New("failed to generate state parameter")
	ErrCallbackUnavailable  = errors.New("callback server unavailable")
	ErrCallbackStart        = errors.New("failed to start callback server")
)

const profileResponseLimit int64 = 64 << 10

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
type AuthCodeURLFunc func(*oauth2.Config, string) string
type ExchangeTokenFunc func(context.Context, *oauth2.Config, string) (*oauth2.Token, error)
type UserInfoFetcher func(context.Context, *oauth2.Config, *oauth2.Token) (UserInfoResult, error)
type AuthenticatedClientFunc func(context.Context, *geminiauth.GeminiTokenStorage, *config.Config) (*http.Client, error)
type ProjectConfiguratorFunc func(context.Context, *http.Client, *geminiauth.GeminiTokenStorage, string) error

type OAuthLoginOptions struct {
	Config              *config.Config
	AuthDir             string
	ProjectID           string
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
	AuthCodeURL         AuthCodeURLFunc
	ExchangeToken       ExchangeTokenFunc
	FetchUserInfo       UserInfoFetcher
	AuthenticatedClient AuthenticatedClientFunc
	ConfigureProject    ProjectConfiguratorFunc
}

type OAuthLoginResult struct {
	AuthURL string
	State   string
}

type UserInfoResult struct {
	Email    string
	TokenMap map[string]any
}

type SessionError struct {
	Message string
	Err     error
}

func (e *SessionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Err)
}

func (e *SessionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func StartOAuthLogin(ctx context.Context, opts OAuthLoginOptions) (OAuthLoginResult, error) {
	ctx = withProxyHTTPClient(ctx, opts.Config)

	fmt.Println("Initializing Google authentication...")

	clientID, clientSecret := "", ""
	if opts.Config != nil {
		clientID, clientSecret = opts.Config.OAuthClientCredentials(config.OAuthClientGemini)
	}
	if strings.TrimSpace(clientID) == "" {
		return OAuthLoginResult{}, ErrOAuthClientIDMissing
	}

	callbackPort := opts.CallbackPort
	if callbackPort == 0 {
		callbackPort = geminiauth.DefaultCallbackPort
	}
	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiauth.DefaultCallbackPort),
		Scopes:       geminiauth.Scopes,
		Endpoint:     google.Endpoint,
	}

	generateState := opts.GenerateState
	if generateState == nil {
		generateState = misc.GenerateRandomState
	}
	state, errState := generateState()
	if errState != nil {
		return OAuthLoginResult{}, fmt.Errorf("%w: %v", ErrStateGeneration, errState)
	}

	opts.Sessions.register(state, "gemini")

	forwarder, actualPort, errForwarder := startCallbackForwarder(opts, callbackPort)
	if errForwarder != nil {
		return OAuthLoginResult{}, errForwarder
	}
	conf.RedirectURL = fmt.Sprintf("http://localhost:%d/oauth2callback", actualPort)

	authCodeURL := opts.AuthCodeURL
	if authCodeURL == nil {
		authCodeURL = defaultAuthCodeURL
	}
	authURL := authCodeURL(conf, state)

	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}
	callbackWaitTimeout := opts.CallbackWaitTimeout
	if callbackWaitTimeout == 0 {
		callbackWaitTimeout = oauthsession.DefaultTTL
	}

	go func() {
		defer stopForwarder(ctx, opts, actualPort, forwarder)

		fmt.Println("Waiting for authentication callback...")
		waitCallback := opts.WaitCallback
		if waitCallback == nil {
			log.Error("oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		resultMap, errWait := waitCallback(authDir, "gemini", state, callbackWaitTimeout)
		if errWait != nil {
			if errors.Is(errWait, oauthsession.ErrNotPending) {
				return
			}
			log.Error("oauth flow timed out")
			opts.Sessions.setError(state, "OAuth flow timed out")
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			log.Errorf("Authentication failed: %s", errStr)
			opts.Sessions.setError(state, "Authentication failed")
			return
		}
		authCode := resultMap["code"]
		if authCode == "" {
			log.Errorf("Authentication failed: code not found")
			opts.Sessions.setError(state, "Authentication failed: code not found")
			return
		}

		exchangeToken := opts.ExchangeToken
		if exchangeToken == nil {
			exchangeToken = defaultExchangeToken
		}
		token, errExchange := exchangeToken(ctx, conf, authCode)
		if errExchange != nil {
			log.Errorf("Failed to exchange token: %v", errExchange)
			opts.Sessions.setError(state, "Failed to exchange token")
			return
		}

		fetchUserInfo := opts.FetchUserInfo
		if fetchUserInfo == nil {
			fetchUserInfo = defaultFetchUserInfo
		}
		info, errInfo := fetchUserInfo(ctx, conf, token)
		if errInfo != nil {
			logGeminiSessionError(errInfo)
			opts.Sessions.setError(state, sessionMessage(errInfo, "Could not get user info"))
			return
		}

		requestedProjectID := strings.TrimSpace(opts.ProjectID)
		ts := geminiauth.GeminiTokenStorage{
			Token:     geminiauth.EnrichOAuthTokenMap(info.TokenMap, conf),
			ProjectID: requestedProjectID,
			Email:     info.Email,
			Auto:      requestedProjectID == "",
		}

		authenticatedClient := opts.AuthenticatedClient
		if authenticatedClient == nil {
			authenticatedClient = defaultAuthenticatedClient
		}
		gemClient, errGetClient := authenticatedClient(ctx, &ts, opts.Config)
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			opts.Sessions.setError(state, "Failed to get authenticated client")
			return
		}
		fmt.Println("Authentication successful.")

		configureProject := opts.ConfigureProject
		if configureProject == nil {
			configureProject = defaultConfigureProject
		}
		if errConfigure := configureProject(ctx, gemClient, &ts, requestedProjectID); errConfigure != nil {
			logGeminiSessionError(errConfigure)
			opts.Sessions.setError(state, sessionMessage(errConfigure, "Failed to complete Gemini CLI onboarding"))
			return
		}

		record := RecordFromTokenStorage(&ts)
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
		opts.Sessions.completeProvider("gemini")
		fmt.Printf("You can now use Gemini CLI services through this CLI; token saved to %s\n", savedPath)
	}()

	return OAuthLoginResult{AuthURL: authURL, State: state}, nil
}

func withProxyHTTPClient(ctx context.Context, cfg *config.Config) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return ctx
	}
	proxyHTTPClient := util.SetProxy(&cfg.SDKConfig, util.NewHTTPClient(util.DefaultHTTPClientTimeout))
	return context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)
}

func startCallbackForwarder(opts OAuthLoginOptions, callbackPort int) (CallbackForwarder, int, error) {
	if !opts.WebUI {
		return nil, callbackPort, nil
	}
	if opts.CallbackTarget == nil {
		return nil, callbackPort, ErrCallbackUnavailable
	}
	targetURL, errTarget := opts.CallbackTarget("/google/callback")
	if errTarget != nil {
		return nil, callbackPort, fmt.Errorf("%w: %v", ErrCallbackUnavailable, errTarget)
	}
	start := opts.StartForwarder
	if start == nil {
		start = defaultStartForwarder
	}
	forwarder, actualPort, errStart := start(callbackPort, "gemini", targetURL)
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

func defaultAuthCodeURL(conf *oauth2.Config, state string) string {
	return conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
}

func defaultExchangeToken(ctx context.Context, conf *oauth2.Config, authCode string) (*oauth2.Token, error) {
	return conf.Exchange(ctx, authCode)
}

func defaultFetchUserInfo(ctx context.Context, conf *oauth2.Config, token *oauth2.Token) (UserInfoResult, error) {
	authHTTPClient := conf.Client(ctx, token)
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	if errRequest != nil {
		return UserInfoResult{}, &SessionError{Message: "Could not get user info", Err: errRequest}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

	resp, errDo := authHTTPClient.Do(req)
	if errDo != nil {
		return UserInfoResult{}, &SessionError{Message: "Failed to execute request", Err: errDo}
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Printf("warn: failed to close response body: %v", errClose)
		}
	}()

	bodyBytes, errReadBody := bodyutil.ReadAll(resp.Body, profileResponseLimit)
	if errReadBody != nil {
		if bodyutil.IsTooLarge(errReadBody) {
			return UserInfoResult{}, &SessionError{Message: "Get user info response too large", Err: errReadBody}
		}
		return UserInfoResult{}, &SessionError{Message: "Could not read user info response", Err: errReadBody}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return UserInfoResult{}, &SessionError{
			Message: fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode),
			Err:     fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes)),
		}
	}

	email := gjson.GetBytes(bodyBytes, "email").String()
	if email != "" {
		fmt.Printf("Authenticated user email: %s\n", email)
	} else {
		fmt.Println("Failed to get user email from token")
	}

	var tokenMap map[string]any
	jsonData, _ := json.Marshal(token)
	if errUnmarshal := json.Unmarshal(jsonData, &tokenMap); errUnmarshal != nil {
		return UserInfoResult{}, &SessionError{Message: "Failed to unmarshal token", Err: errUnmarshal}
	}

	return UserInfoResult{Email: email, TokenMap: tokenMap}, nil
}

func defaultAuthenticatedClient(ctx context.Context, storage *geminiauth.GeminiTokenStorage, cfg *config.Config) (*http.Client, error) {
	gemAuth := geminiauth.NewGeminiAuth()
	return gemAuth.GetAuthenticatedClient(ctx, storage, cfg, &geminiauth.WebLoginOptions{NoBrowser: true})
}

func defaultConfigureProject(ctx context.Context, httpClient *http.Client, storage *geminiauth.GeminiTokenStorage, requestedProjectID string) error {
	if strings.EqualFold(requestedProjectID, "ALL") {
		storage.Auto = false
		projects, errAll := OnboardAllProjects(ctx, httpClient, storage)
		if errAll != nil {
			return &SessionError{Message: "Failed to complete Gemini CLI onboarding", Err: errAll}
		}
		if errVerify := EnsureProjectsEnabled(ctx, httpClient, projects); errVerify != nil {
			return &SessionError{Message: "Failed to verify Cloud AI API status", Err: errVerify}
		}
		storage.ProjectID = strings.Join(projects, ",")
		storage.Checked = true
		return nil
	}

	if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
		storage.Auto = false
		if errSetup := PerformSetup(ctx, httpClient, storage, ""); errSetup != nil {
			return &SessionError{Message: "Google One auto-discovery failed", Err: errSetup}
		}
		if strings.TrimSpace(storage.ProjectID) == "" {
			return &SessionError{Message: "Google One auto-discovery returned empty project ID"}
		}
		isChecked, errCheck := CheckCloudAPIIsEnabled(ctx, httpClient, storage.ProjectID)
		if errCheck != nil {
			return &SessionError{Message: "Failed to verify Cloud AI API status", Err: errCheck}
		}
		storage.Checked = isChecked
		if !isChecked {
			return &SessionError{Message: "Cloud AI API not enabled"}
		}
		return nil
	}

	if errEnsure := EnsureProjectAndOnboard(ctx, httpClient, storage, requestedProjectID); errEnsure != nil {
		return &SessionError{Message: "Failed to complete Gemini CLI onboarding", Err: errEnsure}
	}
	if strings.TrimSpace(storage.ProjectID) == "" {
		return &SessionError{Message: "Failed to resolve project ID"}
	}
	isChecked, errCheck := CheckCloudAPIIsEnabled(ctx, httpClient, storage.ProjectID)
	if errCheck != nil {
		return &SessionError{Message: "Failed to verify Cloud AI API status", Err: errCheck}
	}
	storage.Checked = isChecked
	if !isChecked {
		return &SessionError{Message: "Cloud AI API not enabled"}
	}
	return nil
}

func logGeminiSessionError(err error) {
	var sessionErr *SessionError
	if errors.As(err, &sessionErr) {
		if sessionErr.Err != nil {
			log.Errorf("%s: %v", sessionErr.Message, sessionErr.Err)
			return
		}
		log.Error(sessionErr.Message)
		return
	}
	log.Error(err)
}

func sessionMessage(err error, fallback string) string {
	var sessionErr *SessionError
	if errors.As(err, &sessionErr) && strings.TrimSpace(sessionErr.Message) != "" {
		return sessionErr.Message
	}
	return fallback
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
