package management

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestInFlightViewHidesWhoASharedAccountIs(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	shared := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Label: "owner@example.com"}
	own := &coreauth.Auth{ID: "tenant-a/own.json", Provider: "claude", Label: "mine@example.com", TenantID: sharingTenantA}
	for _, auth := range []*coreauth.Auth{shared, own} {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	mgr.SetSharedAuthGrants(map[string][]string{shared.ID: {sharingTenantA}})
	h := (&Handler{authManager: mgr}).UsageLogs()

	requests := []diagnostics.InFlightRequest{
		{Account: &diagnostics.InFlightAccount{AuthID: shared.ID, Label: "owner@example.com"}},
		{Account: &diagnostics.InFlightAccount{AuthID: own.ID, Label: "mine@example.com"}},
		{},
	}
	h.maskSharedInFlightAccounts(sharingTenantA, requests)
	if got := requests[0].Account; strings.Contains(got.AuthID+got.Label, "owner") || got.AuthID != shared.SharedID() || got.Label != shared.SharedName() {
		t.Fatalf("shared account in flight = %+v", got)
	}
	if got := requests[1].Account; got.AuthID != own.ID || got.Label != "mine@example.com" {
		t.Fatalf("the tenant's own account was renamed: %+v", got)
	}
}

// The in-memory statistics are one aggregate of every tenant's requests.
func TestProcessWideUsageIsPlatformOnly(t *testing.T) {
	h := &Handler{}
	for name, serve := range map[string]func(caller sharingCaller) int{
		"usage": func(caller sharingCaller) int {
			return serveSharing(t, caller, http.MethodGet, "/usage", "", nil, h.GetUsageStatistics).Code
		},
		"export": func(caller sharingCaller) int {
			return serveSharing(t, caller, http.MethodGet, "/usage/export", "", nil, h.ExportUsageStatistics).Code
		},
		"import": func(caller sharingCaller) int {
			return serveSharing(t, caller, http.MethodPost, "/usage/import", "{}", nil, h.ImportUsageStatistics).Code
		},
	} {
		if code := serve(tenantAAdmin); code != http.StatusForbidden {
			t.Fatalf("%s for a tenant = %d, want 403", name, code)
		}
		if code := serve(platformAdmin); code == http.StatusForbidden {
			t.Fatalf("%s for the platform = 403", name)
		}
	}
}
