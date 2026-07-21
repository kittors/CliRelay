package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

func TestResolveBootstrapAdminPasswordPriority(t *testing.T) {
	tests := []struct {
		name               string
		clirelayPassword   string
		managementPassword string
		secretKey          string
		wantValue          string
		wantSource         string
	}{
		{
			name:               "clirelay env wins",
			clirelayPassword:   "clirelay-password",
			managementPassword: "management-password",
			secretKey:          "config-password",
			wantValue:          "clirelay-password",
			wantSource:         "CLIRELAY_ADMIN_PASSWORD",
		},
		{
			name:               "management env fallback",
			managementPassword: "management-password",
			secretKey:          "config-password",
			wantValue:          "management-password",
			wantSource:         "MANAGEMENT_PASSWORD",
		},
		{
			name:       "config fallback",
			secretKey:  "config-password",
			wantValue:  "config-password",
			wantSource: "remote-management.secret-key",
		},
		{
			name:       "empty config fallback",
			wantSource: "remote-management.secret-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLIRELAY_ADMIN_PASSWORD", tt.clirelayPassword)
			t.Setenv("MANAGEMENT_PASSWORD", tt.managementPassword)
			cfg := &config.Config{}
			cfg.RemoteManagement.SecretKey = tt.secretKey

			got := resolveBootstrapAdminPassword(cfg)
			if got.value != tt.wantValue || got.source != tt.wantSource {
				t.Fatalf("resolveBootstrapAdminPassword() = %#v, want value %q from %q", got, tt.wantValue, tt.wantSource)
			}
		})
	}
}

func TestBootstrapIdentityErrorIncludesPasswordSourceWithoutValue(t *testing.T) {
	const passwordValue = "do-not-log-this-password"
	resolved := bootstrapAdminPassword{
		value:  passwordValue,
		source: "CLIRELAY_ADMIN_PASSWORD",
	}
	service := identityBootstrapFunc(func(context.Context, string) error {
		return errors.New("bootstrap failed")
	})

	err := bootstrapIdentity(context.Background(), service, resolved)
	if err == nil {
		t.Fatal("bootstrapIdentity() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), resolved.source) {
		t.Fatalf("bootstrapIdentity() error = %q, want source %q", err, resolved.source)
	}
	if strings.Contains(err.Error(), passwordValue) {
		t.Fatalf("bootstrapIdentity() error leaked password value: %q", err)
	}
}

func TestBootstrapIdentityEmptyPasswordErrorIncludesConfigSource(t *testing.T) {
	t.Setenv("CLIRELAY_ADMIN_PASSWORD", "")
	t.Setenv("MANAGEMENT_PASSWORD", "")
	resolved := resolveBootstrapAdminPassword(&config.Config{})
	service := identityBootstrapFunc(func(_ context.Context, password string) error {
		_, err := identity.HashPassword(password)
		return err
	})

	err := bootstrapIdentity(context.Background(), service, resolved)
	if err == nil {
		t.Fatal("bootstrapIdentity() error = nil, want validation failure")
	}
	if !errors.Is(err, identity.ErrValidation) {
		t.Fatalf("bootstrapIdentity() error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "remote-management.secret-key") {
		t.Fatalf("bootstrapIdentity() error = %q, want config source", err)
	}
}

type identityBootstrapFunc func(context.Context, string) error

func (f identityBootstrapFunc) Bootstrap(ctx context.Context, password string) error {
	return f(ctx, password)
}
