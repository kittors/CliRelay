package management

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// copiesView reads the copies_elsewhere fields of the caller's own files, by id.
func copiesView(t *testing.T, h *Handler, caller sharingCaller) map[string]map[string]any {
	t.Helper()
	w := serveSharing(t, caller, http.MethodGet, "/auth-files", "", nil, h.ListAuthFiles)
	var view struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	out := make(map[string]map[string]any, len(view.Files))
	for _, file := range view.Files {
		id, _ := file["id"].(string)
		out[id] = file
	}
	return out
}

func TestAuthFilesCountCopiesOfTheAccountInOtherTenants(t *testing.T) {
	h := setupSharingTest(t)
	register := func(id, tenantID, provider string, metadata map[string]any, disabled bool) {
		t.Helper()
		auth := &coreauth.Auth{
			ID:         id,
			TenantID:   tenantID,
			Provider:   provider,
			Status:     coreauth.StatusActive,
			Disabled:   disabled,
			Attributes: map[string]string{"path": filepath.Join(t.TempDir(), "creds.json")},
			Metadata:   metadata,
		}
		if disabled {
			auth.Status = coreauth.StatusDisabled
		}
		if _, err := h.authManager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	account := func() map[string]any {
		return map[string]any{"account_id": "acct-dup-1", "email": "ops@example.com"}
	}
	// One upstream account imported by tenant A, tenant B and the platform.
	register(sharingTenantA+"/codex-a.json", sharingTenantA, "codex", account(), false)
	register(sharingTenantB+"/codex-b.json", sharingTenantB, "codex", account(), false)
	register("codex-platform.json", identity.SystemTenantID, "codex", account(), false)
	// Another account, held elsewhere only by a disabled credential.
	register(sharingTenantA+"/claude-a.json", sharingTenantA, "claude", map[string]any{"email": "dev@example.com"}, false)
	register(sharingTenantB+"/claude-b.json", sharingTenantB, "claude", map[string]any{"email": "dev@example.com"}, true)
	// One API key mounted by both tenants: no refresh token to fight over.
	register(sharingTenantA+"/openai:apikey:0123456789abcdef", sharingTenantA, "openai", nil, false)
	register(sharingTenantB+"/openai:apikey:0123456789abcdef", sharingTenantB, "openai", nil, false)

	files := copiesView(t, h, tenantAAdmin)
	codex := files[sharingTenantA+"/codex-a.json"]
	if codex == nil || codex["copies_elsewhere"] != float64(2) {
		t.Fatalf("tenant A's codex file = %v, want 2 copies elsewhere", codex)
	}
	if _, named := codex["copies_elsewhere_tenants"]; named {
		t.Fatalf("a tenant learned which other tenants hold its account: %v", codex["copies_elsewhere_tenants"])
	}
	for _, id := range []string{sharingTenantA + "/claude-a.json", sharingTenantA + "/openai:apikey:0123456789abcdef"} {
		if file := files[id]; file == nil || file["copies_elsewhere"] != nil {
			t.Fatalf("%s = %v, want no copies counted", id, file)
		}
	}

	// A platform administrator acting in tenant A is told who.
	admin := sharingCaller{tenantID: sharingTenantA, platformAdmin: true}
	codex = copiesView(t, h, admin)[sharingTenantA+"/codex-a.json"]
	refs, _ := codex["copies_elsewhere_tenants"].([]any)
	if len(refs) != 2 {
		t.Fatalf("platform view of tenant A = %v", codex)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		entry, _ := ref.(map[string]any)
		names[entry["name"].(string)] = true
	}
	if !names["Tenant B"] || !names["System Administration"] {
		t.Fatalf("named tenants = %v", refs)
	}

	// Once the platform shares its copy with tenant A, that one is the shared
	// duplicate, and only tenant B's counts as elsewhere.
	if _, err := usage.ReplaceResourceGrants(context.Background(), resourcegrants.KindAuth, []string{"codex-platform.json"}, []string{sharingTenantA}); err != nil {
		t.Fatal(err)
	}
	h.reloadSharedAuthGrants()
	codex = copiesView(t, h, tenantAAdmin)[sharingTenantA+"/codex-a.json"]
	if codex["duplicates_shared"] != true || codex["copies_elsewhere"] != float64(1) {
		t.Fatalf("after sharing: %v", codex)
	}
}
