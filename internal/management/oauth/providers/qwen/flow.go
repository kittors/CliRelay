package qwen

import (
	"context"
	"fmt"
	"time"

	internalqwen "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/qwen"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type DeviceAuth interface {
	InitiateDeviceFlow(context.Context) (*internalqwen.DeviceFlow, error)
	PollForToken(deviceCode, codeVerifier string) (*internalqwen.QwenTokenData, error)
	CreateTokenStorage(*internalqwen.QwenTokenData) *internalqwen.QwenTokenStorage
}

type SessionCallbacks struct {
	Register func(state, provider string)
	SetError func(state, message string)
	Complete func(state string)
}

type SaveRecordFunc func(context.Context, *coreauth.Auth) (string, error)

type DeviceLoginOptions struct {
	Config     *config.Config
	Auth       DeviceAuth
	Sessions   SessionCallbacks
	SaveRecord SaveRecordFunc
	Now        func() time.Time
	State      func() string
}

type DeviceLoginResult struct {
	AuthURL string
	State   string
	// UserCode, VerificationURI and ExpiresIn come straight from the device
	// authorization response so the panel can show the code the verification
	// page asks for and count down its lifetime, instead of only a link.
	UserCode        string
	VerificationURI string
	ExpiresIn       int
}

func StartDeviceLogin(ctx context.Context, opts DeviceLoginOptions) (DeviceLoginResult, error) {
	fmt.Println("Initializing Qwen authentication...")

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	state := ""
	if opts.State != nil {
		state = opts.State()
	}
	if state == "" {
		state = fmt.Sprintf("gem-%d", now().UnixNano())
	}
	auth := opts.Auth
	if auth == nil {
		auth = internalqwen.NewQwenAuth(opts.Config)
	}

	deviceFlow, err := auth.InitiateDeviceFlow(ctx)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	result := DeviceLoginResult{
		AuthURL:         deviceFlow.VerificationURIComplete,
		State:           state,
		UserCode:        deviceFlow.UserCode,
		VerificationURI: deviceFlow.VerificationURI,
		ExpiresIn:       deviceFlow.ExpiresIn,
	}
	opts.Sessions.register(state, "qwen")

	go func() {
		fmt.Println("Waiting for authentication...")
		tokenData, errPollForToken := auth.PollForToken(deviceFlow.DeviceCode, deviceFlow.CodeVerifier)
		if errPollForToken != nil {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errPollForToken)
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
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			opts.Sessions.setError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Qwen services through this CLI")
		opts.Sessions.complete(state)
	}()

	return result, nil
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
