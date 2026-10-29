package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestRuntimeDataStackPostStartHooksRunMaintenanceAsynchronously(t *testing.T) {
	sharedBackfillStarted := make(chan struct{})
	releaseSharedBackfill := make(chan struct{})
	sharedBackfillRan := make(chan struct{}, 1)
	catchupScheduled := make(chan struct{}, 1)
	keyMaskingScheduled := make(chan struct{}, 1)
	sessionBackfillScheduled := make(chan struct{}, 1)

	hooks := runtimeDataStackPostStartHooks(runtimeDataStackMaintenanceOps{
		runAIAccountSharedSubjectBackfill: func() error {
			close(sharedBackfillStarted)
			<-releaseSharedBackfill
			sharedBackfillRan <- struct{}{}
			return nil
		},
		scheduleUsageRollupCatchup: func() {
			catchupScheduled <- struct{}{}
		},
		scheduleRequestLogKeyMasking: func() {
			keyMaskingScheduled <- struct{}{}
		},
		scheduleRequestLogSessionBackfill: func() {
			sessionBackfillScheduled <- struct{}{}
		},
	})

	select {
	case <-sharedBackfillStarted:
		t.Fatal("maintenance started before OnAfterStart")
	default:
	}

	hookReturned := make(chan struct{})
	go func() {
		hooks.OnAfterStart(nil)
		close(hookReturned)
	}()

	select {
	case <-hookReturned:
	case <-time.After(time.Second):
		t.Fatal("OnAfterStart blocked on runtime data maintenance")
	}
	select {
	case <-sharedBackfillStarted:
	case <-time.After(time.Second):
		t.Fatal("post-start shared subject backfill did not start")
	}
	select {
	case <-sharedBackfillRan:
		t.Fatal("shared subject backfill completed before it was released")
	default:
	}
	select {
	case <-keyMaskingScheduled:
		t.Fatal("request log key masking was scheduled before the repairs finished")
	case <-sessionBackfillScheduled:
		t.Fatal("request log session backfill was scheduled before the repairs finished")
	default:
	}

	close(releaseSharedBackfill)
	select {
	case <-sharedBackfillRan:
	case <-time.After(time.Second):
		t.Fatal("shared subject backfill did not complete after release")
	}
	select {
	case <-catchupScheduled:
	case <-time.After(time.Second):
		t.Fatal("blue-green rollup catch-up was not scheduled")
	}
	select {
	case <-keyMaskingScheduled:
	case <-time.After(time.Second):
		t.Fatal("request log key masking was not scheduled")
	}
	select {
	case <-sessionBackfillScheduled:
	case <-time.After(time.Second):
		t.Fatal("request log session backfill was not scheduled")
	}
}

type identityBootstrapFunc func(context.Context, string) error

func (f identityBootstrapFunc) Bootstrap(ctx context.Context, password string) error {
	return f(ctx, password)
}
