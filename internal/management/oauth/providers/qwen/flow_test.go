package qwen

import (
	"context"
	"errors"
	"testing"
	"time"

	internalqwen "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qwen"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type qwenDeviceAuthStub struct {
	initiateErr error
	pollErr     error
}

func (s qwenDeviceAuthStub) InitiateDeviceFlow(context.Context) (*internalqwen.DeviceFlow, error) {
	if s.initiateErr != nil {
		return nil, s.initiateErr
	}
	return &internalqwen.DeviceFlow{
		DeviceCode:              "device-code",
		CodeVerifier:            "code-verifier",
		VerificationURIComplete: "https://example.com/qwen",
	}, nil
}

func (s qwenDeviceAuthStub) PollForToken(deviceCode, codeVerifier string) (*internalqwen.QwenTokenData, error) {
	if deviceCode != "device-code" || codeVerifier != "code-verifier" {
		return nil, errors.New("unexpected device flow values")
	}
	if s.pollErr != nil {
		return nil, s.pollErr
	}
	return &internalqwen.QwenTokenData{
		AccessToken: "access",
		TokenType:   "Bearer",
	}, nil
}

func (s qwenDeviceAuthStub) CreateTokenStorage(tokenData *internalqwen.QwenTokenData) *internalqwen.QwenTokenStorage {
	return &internalqwen.QwenTokenStorage{
		AccessToken: tokenData.AccessToken,
		Type:        "qwen",
	}
}

func TestStartDeviceLoginRegistersAndCompletesSession(t *testing.T) {
	done := make(chan struct{})
	var registeredState string
	var savedProvider string

	result, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth: qwenDeviceAuthStub{},
		Now: func() time.Time {
			return time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
		},
		State: func() string { return "qwen-state" },
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registeredState = state + ":" + provider
			},
			Complete: func(state string) {
				if state != "qwen-state" {
					t.Errorf("complete state = %q, want qwen-state", state)
				}
				close(done)
			},
		},
		SaveRecord: func(ctx context.Context, record *coreauth.Auth) (string, error) {
			_ = ctx
			if record == nil {
				t.Fatal("record is nil")
			}
			savedProvider = record.Provider
			return "/tmp/qwen.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/qwen" || result.State != "qwen-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if registeredState != "qwen-state:qwen" {
		t.Fatalf("registered = %q, want qwen-state:qwen", registeredState)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for completion")
	}
	if savedProvider != "qwen" {
		t.Fatalf("saved provider = %q, want qwen", savedProvider)
	}
}

func TestStartDeviceLoginSetsErrorOnPollFailure(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth:  qwenDeviceAuthStub{pollErr: errors.New("denied")},
		State: func() string { return "qwen-state" },
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "qwen-state" {
					t.Errorf("error state = %q, want qwen-state", state)
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

func TestStartDeviceLoginReturnsInitiateError(t *testing.T) {
	wantErr := errors.New("network")
	_, err := StartDeviceLogin(context.Background(), DeviceLoginOptions{
		Auth: qwenDeviceAuthStub{initiateErr: wantErr},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("StartDeviceLogin() error = %v, want %v", err, wantErr)
	}
}
