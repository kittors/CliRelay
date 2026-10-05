package kimi

import (
	"context"
	"fmt"
	"time"

	internalkimi "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type DeviceAuth interface {
	StartDeviceFlow(context.Context) (*internalkimi.DeviceCodeResponse, error)
	WaitForAuthorization(context.Context, *internalkimi.DeviceCodeResponse) (*internalkimi.KimiAuthBundle, error)
	CreateTokenStorage(*internalkimi.KimiAuthBundle) *internalkimi.KimiTokenStorage
}

type SessionCallbacks struct {
	Register         func(state, provider string)
	SetError         func(state, message string)
	Complete         func(state string)
	CompleteProvider func(provider string) int
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
	fmt.Println("Initializing Kimi authentication...")

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	state := ""
	if opts.State != nil {
		state = opts.State()
	}
	if state == "" {
		state = fmt.Sprintf("kmi-%d", now().UnixNano())
	}
	auth := opts.Auth
	if auth == nil {
		auth = internalkimi.NewKimiAuth(opts.Config)
	}

	deviceFlow, err := auth.StartDeviceFlow(ctx)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	authURL := deviceFlow.VerificationURIComplete
	if authURL == "" {
		authURL = deviceFlow.VerificationURI
	}
	result := DeviceLoginResult{
		AuthURL:         authURL,
		State:           state,
		UserCode:        deviceFlow.UserCode,
		VerificationURI: deviceFlow.VerificationURI,
		ExpiresIn:       deviceFlow.ExpiresIn,
	}
	opts.Sessions.register(state, "kimi")

	go func() {
		fmt.Println("Waiting for authentication...")
		authBundle, errWaitForAuthorization := auth.WaitForAuthorization(ctx, deviceFlow)
		if errWaitForAuthorization != nil {
			opts.Sessions.setError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errWaitForAuthorization)
			return
		}

		tokenStorage := auth.CreateTokenStorage(authBundle)
		record := RecordFromAuthBundle(tokenStorage, authBundle, now())
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
		fmt.Println("You can now use Kimi services through this CLI")
		opts.Sessions.complete(state)
		opts.Sessions.completeProvider("kimi")
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

func (s SessionCallbacks) completeProvider(provider string) int {
	if s.CompleteProvider == nil {
		return 0
	}
	return s.CompleteProvider(provider)
}
