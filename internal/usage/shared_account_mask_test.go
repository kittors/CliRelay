package usage

import (
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type fakeSharedAccounts map[string][]*coreauth.Auth

func (f fakeSharedAccounts) ListSharedWithTenant(tenantID string) []*coreauth.Auth {
	return f[tenantID]
}

func TestRequestLogRowsHideTheSharedAccount(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	shared := &coreauth.Auth{ID: "claude-owner@example.com.json", FileName: "claude-owner@example.com.json", Provider: "claude",
		Label: "owner@example.com", Metadata: map[string]any{"email": "owner@example.com"}}
	InstallSharedAccountMasking(fakeSharedAccounts{tenant: {shared}})
	t.Cleanup(func() { InstallSharedAccountMasking(nil) })

	detail, _ := json.Marshal(map[string]any{
		"diagnostic":  map[string]any{"upstream": map[string]any{"auth_id": shared.ID, "auth_label": "owner@example.com"}},
		"route_trace": map[string]any{"steps": []any{map[string]any{"auth_id": shared.ID, "auth_label": "owner@example.com"}}},
		"upstream":    map[string]any{"request_log": "Auth: provider=claude, auth_id=claude-owner@example.com.json, label=owner@example.com"},
	})
	row := RequestLogEntry{
		AuthIndex:     shared.EnsureIndex(),
		Source:        "owner@example.com",
		ChannelName:   "owner@example.com",
		DetailContent: string(detail),
		Failed:        true,
		OutputContent: `{"error":{"message":"account owner@example.com is over its limit"}}`,
	}
	masked := row
	maskSharedAccountRow(&masked, tenant)
	if masked.Source != shared.SharedName() || masked.ChannelName != shared.SharedName() {
		t.Fatalf("source %q, channel %q, want %q", masked.Source, masked.ChannelName, shared.SharedName())
	}
	for _, text := range []string{masked.DetailContent, masked.OutputContent} {
		if strings.Contains(text, "owner") {
			t.Fatalf("masked row still names the account: %s", text)
		}
		if !json.Valid([]byte(text)) {
			t.Fatalf("masked content is not JSON: %s", text)
		}
	}
	if !strings.Contains(masked.DetailContent, shared.SharedID()) {
		t.Fatalf("details lost the account's stand-in id: %s", masked.DetailContent)
	}

	// The platform's own rows, and other tenants' rows, are stored as written.
	for _, rowTenant := range []string{"", systemTenantID, "22222222-2222-2222-2222-222222222222"} {
		kept := row
		maskSharedAccountRow(&kept, rowTenant)
		if kept.Source != row.Source || kept.DetailContent != row.DetailContent {
			t.Fatalf("row of tenant %q was rewritten", rowTenant)
		}
	}
}
