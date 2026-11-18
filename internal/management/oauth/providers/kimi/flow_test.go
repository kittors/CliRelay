package kimi

import (
	"context"
	"errors"
	"testing"
	"time"

	internalkimi "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type kimiDeviceAuthStub struct {
	startErr error
	waitErr  error
}

func (s kimiDeviceAuthStub) StartDeviceFlow(context.Context) (*internalkimi.DeviceCodeResponse, error) {
	if s.startErr != nil {
		return nil, s.startErr
	}
	return &internalkimi.DeviceCodeResponse{
		DeviceCode:              "device-code",
		VerificationURIComplete: "https://example.com/kimi",
		VerificationURI:         "https://example.com/kimi-fallback",
	}, nil
}

func (s kimiDeviceAuthStub) WaitForAuthorization(ctx context.Context, deviceFlow *internalkimi.DeviceCodeResponse) (*internalkimi.KimiAuthBundle, error) {
	_ = ctx
	if deviceFlow == nil || deviceFlow.DeviceCode != "device-code" {
		return nil, errors.New("unexpected device flow")
	}
	if s.waitErr != nil {
		return nil, s.waitErr
	}
	return &internalkimi.KimiAuthBundle{
		TokenData: &internalkimi.KimiTokenData{
			AccessToken:  "access",
			RefreshToken: "refresh",
			TokenType:    "Bearer",
		},
		DeviceID: "device-id",
	}, nil
}

func (s kimiDeviceAuthStub) CreateTokenStorage(bundle *internalkimi.KimiAuthBundle) *internalkimi.KimiTokenStorage {
	return &internalkimi.KimiTokenStorage{
		AccessToken: bundle.TokenData.AccessToken,
		DeviceID:    bundle.DeviceID,
		Type:        "kimi",
	}
}

func TestStartDeviceLoginRegistersCompletesAndCompletesProvider(t *testing.T) {
	done := make(chan struct{})
	var registered string
	var completedProvider string
	var savedProvider string

	result, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth: kimiDeviceAuthStub{},
		Now: func() time.Time {
			return time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
		},
		State: func() string { return "kimi-state" },
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registered = state + ":" + provider
			},
			Complete: func(state string) {
				if state != "kimi-state" {
					t.Errorf("complete state = %q, want kimi-state", state)
				}
			},
			CompleteProvider: func(provider string) int {
				completedProvider = provider
				close(done)
				return 1
			},
		},
		SaveRecord: func(ctx context.Context, record *coreauth.Auth) (string, error) {
			_ = ctx
			if record == nil {
				t.Fatal("record is nil")
			}
			savedProvider = record.Provider
			return "/tmp/kimi.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/kimi" || result.State != "kimi-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if registered != "kimi-state:kimi" {
		t.Fatalf("registered = %q, want kimi-state:kimi", registered)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for provider completion")
	}
	if savedProvider != "kimi" {
		t.Fatalf("saved provider = %q, want kimi", savedProvider)
	}
	if completedProvider != "kimi" {
		t.Fatalf("completed provider = %q, want kimi", completedProvider)
	}
}

func TestStartDeviceLoginUsesFallbackVerificationURI(t *testing.T) {
	auth := kimiDeviceAuthStub{}
	result, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth:  authWithFallbackURI{DeviceAuth: auth},
		State: func() string { return "kimi-state" },
	})
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/kimi-fallback" {
		t.Fatalf("AuthURL = %q, want fallback URI", result.AuthURL)
	}
}

func TestStartDeviceLoginSetsErrorOnWaitFailure(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth:  kimiDeviceAuthStub{waitErr: errors.New("denied")},
		State: func() string { return "kimi-state" },
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "kimi-state" {
					t.Errorf("error state = %q, want kimi-state", state)
				}
				status = message
				close(done)
			},
		},
		SaveRecord: func(context.Context, *coreauth.Auth) (string, error) {
			t.Fatal("SaveRecord should not be called")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error")
	}
	if status != "Authentication failed" {
		t.Fatalf("status = %q, want Authentication failed", status)
	}
}

func TestStartDeviceLoginReturnsStartError(t *testing.T) {
	wantErr := errors.New("network")
	_, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth: kimiDeviceAuthStub{startErr: wantErr},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("StartDeviceLogin() error = %v, want %v", err, wantErr)
	}
}

type authWithFallbackURI struct {
	DeviceAuth
}

func (a authWithFallbackURI) StartDeviceFlow(ctx context.Context) (*internalkimi.DeviceCodeResponse, error) {
	flow, err := a.DeviceAuth.StartDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}
	flow.VerificationURIComplete = ""
	return flow, nil
}
