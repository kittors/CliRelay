package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// egressTestExecutor names each account's proxy the way the real executors do:
// config.ResolveProxyURL against the tenant configuration it is bound with.
type egressTestExecutor struct {
	provider string
	cfg      *config.Config
}

func (e egressTestExecutor) Identifier() string { return e.provider }

func (e egressTestExecutor) EgressProxyURL(auth *coreauth.Auth) string {
	return e.cfg.ResolveProxyURL(auth.ProxyID, auth.ProxyURL)
}

var errEgressTestExecutor = errors.New("not used by the egress source")

func (egressTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errEgressTestExecutor
}

func (egressTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errEgressTestExecutor
}

func (egressTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (egressTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errEgressTestExecutor
}

func (egressTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errEgressTestExecutor
}

const (
	egressRejecting = "socks5://alice:secret-a@203.0.113.10:1080"
	egressIdle      = "socks5://alice:secret-b@203.0.113.11:1080"
	egressOff       = "socks5://alice:secret-c@203.0.113.12:1080"
	egressShared    = "socks5://platform:secret-d@198.51.100.20:1080"
	egressShadowed  = "socks5://platform:secret-e@198.51.100.21:1080"
	egressDefault   = "http://ops:secret-f@192.0.2.30:3128"
	egressOwn       = "socks5://bob:secret-g@192.0.2.31:1080"
)

// egressTestBreaker returns a breaker on a clock the test moves.
func egressTestBreaker() (*egressbreaker.Breaker, func(time.Duration)) {
	now := time.Date(2026, 11, 11, 16, 20, 0, 0, time.UTC)
	breaker := egressbreaker.New(egressbreaker.Config{Now: func() time.Time { return now }})
	return breaker, func(d time.Duration) { now = now.Add(d) }
}

// rejectThrough fails one handshake through proxyURL on a rejected login.
func rejectThrough(t *testing.T, breaker *egressbreaker.Breaker, proxyURL string) {
	t.Helper()
	permit, err := breaker.Allow(proxyURL)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	_ = permit.Done(egressbreaker.AuthRejected(errors.New("username/password authentication failed")))
}

func TestAlertSourcesEgressTiesTheBreakerToTheTenantsProxies(t *testing.T) {
	const tenant = "tenant-egress-sources"
	own := []config.ProxyPoolEntry{
		{ID: "sg-1", Name: "SG One", URL: egressRejecting, Enabled: true},
		{ID: "sg-2", Name: "SG Two", URL: egressIdle, Enabled: true},
		{ID: "sg-off", Name: "Switched off", URL: egressOff, Enabled: false},
	}
	shared := []config.ProxyPoolEntry{
		{ID: "hk-shared", Name: "HK shared", URL: egressShared, Enabled: true},
		// The tenant's own sg-1 overrides this one.
		{ID: "sg-1", Name: "Platform SG", URL: egressShadowed, Enabled: true},
	}
	tenantCfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: egressDefault}}
	tenantCfg.ProxyPool = config.MergeSharedProxyPool(own, shared)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutorForTenant(tenant, egressTestExecutor{provider: "codex", cfg: tenantCfg})
	manager.RegisterExecutorForTenant("another-tenant", egressTestExecutor{provider: "codex", cfg: tenantCfg})
	register := func(auth *coreauth.Auth) {
		t.Helper()
		if auth.TenantID == "" {
			auth.TenantID = tenant
		}
		if auth.Status == "" {
			auth.Status = coreauth.StatusActive
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	register(&coreauth.Auth{ID: "a1", Provider: "codex", ProxyID: "sg-1"})
	register(&coreauth.Auth{ID: "a2", Provider: "codex", ProxyID: "SG 1"})
	register(&coreauth.Auth{ID: "a3", Provider: "codex", ProxyID: "hk-shared"})
	register(&coreauth.Auth{ID: "a4", Provider: "codex", ProxyURL: egressOwn})
	register(&coreauth.Auth{ID: "a5", Provider: "codex"})
	// Names a switched-off entry, so it leaves through the default proxy.
	register(&coreauth.Auth{ID: "a6", Provider: "codex", ProxyID: "sg-off"})
	// Not counted: switched off, another tenant's, no executor to ask.
	register(&coreauth.Auth{ID: "a7", Provider: "codex", ProxyID: "sg-1", Disabled: true, Status: coreauth.StatusDisabled})
	register(&coreauth.Auth{ID: "a8", Provider: "codex", ProxyID: "sg-1", TenantID: "another-tenant"})
	register(&coreauth.Auth{ID: "a9", Provider: "nobody", ProxyID: "sg-1"})

	breaker, advance := egressTestBreaker()
	opened := time.Date(2026, 11, 11, 16, 20, 0, 0, time.UTC)
	for range egressbreaker.DefaultRejectionThreshold {
		rejectThrough(t, breaker, egressRejecting)
	}
	advance(egressbreaker.DefaultBaseOpen)
	rejectThrough(t, breaker, egressRejecting) // the probe fails
	passed := opened.Add(egressbreaker.DefaultBaseOpen)
	if permit, err := breaker.Allow(egressShared); err != nil {
		t.Fatal(err)
	} else {
		_ = permit.Done(nil)
	}

	sources := alertSources{
		authManager: func() *coreauth.Manager { return manager },
		proxyPools: func(id string) ([]config.ProxyPoolEntry, []config.ProxyPoolEntry) {
			if id != tenant {
				return nil, nil
			}
			return own, shared
		},
		breaker: breaker,
	}
	samples, err := sources.Egress(context.Background(), tenant, opened)
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		kind, id, name, endpoint string
		accounts                 int
		open                     bool
	}
	wants := []want{
		{alerting.EgressPoolEntry, "sg-1", "SG One", "socks5://203.0.113.10:1080", 2, true},
		{alerting.EgressPoolEntry, "sg-2", "SG Two", "socks5://203.0.113.11:1080", 0, false},
		{alerting.EgressSharedEntry, "hk-shared", "HK shared", "socks5://198.51.100.20:1080", 1, false},
		{alerting.EgressAccountProxy, "", "", "socks5://192.0.2.31:1080", 1, false},
		{alerting.EgressDefaultProxy, "", "", "http://192.0.2.30:3128", 2, false},
	}
	if len(samples) != len(wants) {
		t.Fatalf("samples = %+v, want %d", samples, len(wants))
	}
	for i, w := range wants {
		s := samples[i]
		if s.Kind != w.kind || s.ID != w.id || s.Name != w.name || s.Endpoint != w.endpoint || s.Accounts != w.accounts || s.Open != w.open {
			t.Errorf("sample %d = %+v, want %+v", i, s, w)
		}
		if strings.Contains(s.Endpoint+s.Name+s.ID, "secret") || strings.Contains(s.Fingerprint, "secret") || len(s.Fingerprint) != 16 {
			t.Errorf("sample %d carries a credential or a malformed fingerprint: %+v", i, s)
		}
	}
	rejecting := samples[0]
	if rejecting.Fault != "auth_rejected" || !rejecting.Since.Equal(opened) || rejecting.FailedProbes != 1 || rejecting.Fingerprint != egressFingerprint(egressRejecting) {
		t.Fatalf("the open proxy = %+v", rejecting)
	}
	if !samples[2].LastPass.Equal(passed) {
		t.Fatalf("the shared proxy's last pass = %v, want %v", samples[2].LastPass, passed)
	}
	if samples[0].Fingerprint == samples[1].Fingerprint {
		t.Fatal("two addresses share a fingerprint")
	}
}

func TestAlertSourcesEgressWithoutAccountsIsAnError(t *testing.T) {
	for _, sources := range []alertSources{{}, {authManager: func() *coreauth.Manager { return nil }}} {
		samples, err := sources.Egress(context.Background(), "tenant", time.Now())
		if !errors.Is(err, errEgressAccountsUnavailable) || samples != nil {
			t.Fatalf("Egress without an auth manager = %+v, %v; want the error, so no outage is taken for over", samples, err)
		}
	}
}
