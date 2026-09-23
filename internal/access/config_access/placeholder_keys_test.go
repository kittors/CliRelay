package configaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	_ "modernc.org/sqlite"
)

// Every place the provider reads a client key from, so the example keys cannot
// slip in through a header the check forgot.
var credentialCarriers = []struct {
	name  string
	build func(key string) *http.Request
}{
	{"authorization", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		return req
	}},
	{"x-api-key", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("X-Api-Key", key)
		return req
	}},
	{"x-goog-api-key", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:generateContent", nil)
		req.Header.Set("X-Goog-Api-Key", key)
		return req
	}},
	{"query-key", func(key string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:generateContent?key="+key, nil)
	}},
	{"query-auth-token", func(key string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/chat/completions?auth_token="+key, nil)
	}},
}

func assertRejectedAsInvalid(t *testing.T, res *sdkaccess.Result, authErr *sdkaccess.AuthError) {
	t.Helper()
	if authErr == nil {
		t.Fatalf("example key authenticated as %+v, want it rejected", res)
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) || authErr.HTTPStatusCode() != http.StatusUnauthorized {
		t.Fatalf("auth error = %s (%d), want invalid_credential (401) like any unknown key", authErr.Code, authErr.HTTPStatusCode())
	}
}

func useRegisteredProviders(t *testing.T, cfg *config.SDKConfig, allowUnauthenticated bool) *sdkaccess.Manager {
	t.Helper()
	t.Cleanup(func() { sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey) })
	Register(cfg)
	manager := sdkaccess.NewManager()
	manager.SetAllowAllWhenNoProviders(allowUnauthenticated)
	manager.SetProviders(sdkaccess.RegisteredProviders())
	return manager
}

func TestProviderRejectsPlaceholderAPIKeysFromConfig(t *testing.T) {
	cfg := &config.SDKConfig{
		APIKeys:       []string{"your-api-key-1", "sk-real-key"},
		APIKeyEntries: []config.APIKeyEntry{{Key: "your-api-key-2", Name: "example entry"}},
	}
	p := newProvider("test", buildKeyConfigMap(cfg))

	for _, carrier := range credentialCarriers {
		for _, key := range []string{"your-api-key-1", "your-api-key-2"} {
			res, authErr := p.Authenticate(context.Background(), carrier.build(key))
			assertRejectedAsInvalid(t, res, authErr)
		}
		res, authErr := p.Authenticate(context.Background(), carrier.build("sk-real-key"))
		if authErr != nil {
			t.Fatalf("%s: real key rejected: %v", carrier.name, authErr)
		}
		if res.Principal != "sk-real-key" || res.Metadata["source"] != carrier.name {
			t.Fatalf("%s: real key result = %+v", carrier.name, res)
		}
	}
}

// A placeholder rejected in one header must not shadow a real key sent in
// another, the same way an unknown key would not.
func TestProviderSkipsPlaceholderAndAcceptsRealKeyInAnotherHeader(t *testing.T) {
	p := newProvider("test", buildKeyConfigMap(&config.SDKConfig{APIKeys: []string{"your-api-key-1", "sk-real-key"}}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer your-api-key-1")
	req.Header.Set("X-Api-Key", "sk-real-key")

	res, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if res.Principal != "sk-real-key" || res.Metadata["source"] != "x-api-key" {
		t.Fatalf("result = %+v, want the real key from x-api-key", res)
	}
}

// Instances that already imported the example keys keep them as ordinary
// database rows; those rows must stop authenticating too.
func TestProviderRejectsPlaceholderAPIKeysStoredInDatabase(t *testing.T) {
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	for _, row := range []usage.APIKeyRow{
		{Key: "your-api-key-1", Name: "api-key-1"},
		{Key: "your-api-key-3", Name: "api-key-3"},
		{Key: "sk-stored-real", Name: "real"},
	} {
		if err := usage.UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.Name, err)
		}
	}
	manager := useRegisteredProviders(t, &config.SDKConfig{}, false)

	for _, key := range []string{"your-api-key-1", "your-api-key-3"} {
		res, authErr := manager.Authenticate(context.Background(), credentialCarriers[0].build(key))
		assertRejectedAsInvalid(t, res, authErr)
	}
	res, authErr := manager.Authenticate(context.Background(), credentialCarriers[0].build("sk-stored-real"))
	if authErr != nil {
		t.Fatalf("stored real key rejected: %v", authErr)
	}
	if res.APIKeyName != "real" {
		t.Fatalf("stored real key result = %+v", res)
	}
}

// Rejecting the example keys must never make an instance more open than before.
// If they simply dropped out of the key set, an instance holding only them would
// look unconfigured and allow-unauthenticated would let every request through.
func TestProviderWithOnlyPlaceholderKeysStillRequiresAKey(t *testing.T) {
	manager := useRegisteredProviders(t, &config.SDKConfig{APIKeys: []string{"your-api-key-1"}}, true)

	res, authErr := manager.Authenticate(context.Background(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if authErr == nil {
		t.Fatalf("request without a key was let through (result %+v); allow-unauthenticated must not apply while keys are configured", res)
	}
	if authErr.HTTPStatusCode() != http.StatusUnauthorized {
		t.Fatalf("request without a key: status %d, want 401", authErr.HTTPStatusCode())
	}
	res, authErr = manager.Authenticate(context.Background(), credentialCarriers[0].build("your-api-key-1"))
	assertRejectedAsInvalid(t, res, authErr)
}

func TestPlaceholderRejectionWarningIsRateLimitedPerKey(t *testing.T) {
	limiter := &rejectWarnLimiter{interval: time.Minute}
	start := time.Unix(1_700_000_000, 0)
	for _, step := range []struct {
		key  string
		at   time.Duration
		want bool
	}{
		{"your-api-key-1", 0, true},
		{"your-api-key-1", 59 * time.Second, false},
		{"your-api-key-2", 59 * time.Second, true},
		{"your-api-key-1", time.Minute, true},
		{"your-api-key-1", time.Minute + time.Second, false},
	} {
		if got := limiter.allow(step.key, start.Add(step.at)); got != step.want {
			t.Fatalf("allow(%s, +%s) = %v, want %v", step.key, step.at, got, step.want)
		}
	}
}

func warnEntriesMentioning(hook *test.Hook, fragments ...string) int {
	count := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(entry.Message, fragment) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

// The operator needs to hear that a configured key is dead, but a client stuck
// retrying with it must not flood the log.
func TestProviderWarnsOnceWhenRejectingConfiguredPlaceholderKey(t *testing.T) {
	previous := placeholderRejectWarnings
	placeholderRejectWarnings = &rejectWarnLimiter{interval: time.Hour}
	t.Cleanup(func() { placeholderRejectWarnings = previous })
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := newProvider("test", buildKeyConfigMap(&config.SDKConfig{
		APIKeyEntries: []config.APIKeyEntry{{Key: "your-api-key-1", Name: "legacy example"}},
	}))
	for i := 0; i < 3; i++ {
		res, authErr := p.Authenticate(context.Background(), credentialCarriers[0].build("your-api-key-1"))
		assertRejectedAsInvalid(t, res, authErr)
	}
	if n := warnEntriesMentioning(hook, "your-api-key-1", "legacy example", "config.example.yaml"); n != 1 {
		t.Fatalf("got %d warnings for the rejected example key, want exactly 1; entries %v", n, hook.AllEntries())
	}

	// An example key that is not configured is just an unknown key: there is
	// nothing for the operator to replace, so it stays out of the log.
	res, authErr := p.Authenticate(context.Background(), credentialCarriers[0].build("your-api-key-2"))
	assertRejectedAsInvalid(t, res, authErr)
	if n := warnEntriesMentioning(hook, "your-api-key-2"); n != 0 {
		t.Fatalf("got %d warnings for an unconfigured example key, want none", n)
	}
}
