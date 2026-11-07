package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	_ "modernc.org/sqlite"
)

const (
	sharingTenantA = "11111111-1111-1111-1111-111111111111"
	sharingTenantB = "22222222-2222-2222-2222-222222222222"
)

// setupSharingTest gives the handler a usage database (proxy pools and grants)
// and a tenant directory with the system tenant and tenants A and B.
func setupSharingTest(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("init usage db: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	db, err := sql.Open("sqlite", fmt.Sprintf("file:sharing_tenants_%d?mode=memory&cache=shared", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	// ListTenants reads each tenant while its cursor is still open, so one
	// connection would deadlock; the shared-cache DSN keeps them on one database.
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE tenants (
		id TEXT PRIMARY KEY, slug TEXT NOT NULL, name TEXT NOT NULL, type TEXT NOT NULL, status TEXT NOT NULL,
		expires_at TIMESTAMP NULL, description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		version INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, slug, name, kind string }{
		{identity.SystemTenantID, "system", "System Administration", "system"},
		{sharingTenantA, "tenant-a", "Tenant A", "standard"},
		{sharingTenantB, "tenant-b", "Tenant B", "standard"},
	} {
		if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, type, status) VALUES (?, ?, ?, ?, 'active')`, row.id, row.slug, row.name, row.kind); err != nil {
			t.Fatal(err)
		}
	}
	previous := identity.Default()
	identity.SetDefault(identity.NewService(db))
	t.Cleanup(func() { identity.SetDefault(previous) })

	h := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	t.Cleanup(h.Close)
	return h
}

type sharingCaller struct {
	tenantID      string
	platformAdmin bool
}

var (
	platformAdmin = sharingCaller{tenantID: identity.SystemTenantID, platformAdmin: true}
	tenantAAdmin  = sharingCaller{tenantID: sharingTenantA}
	tenantBAdmin  = sharingCaller{tenantID: sharingTenantB}
)

func serveSharing(t *testing.T, caller sharingCaller, method, target, body string, params gin.Params, handler func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   caller.platformAdmin,
		EffectiveTenant: identity.Tenant{ID: caller.tenantID},
		HomeTenant:      identity.Tenant{ID: caller.tenantID},
	})
	handler(c)
	return w
}

func seedSharingProxies(t *testing.T) {
	t.Helper()
	if err := usage.ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: "hk-1", Name: "HK", URL: "socks5://user:secret@203.0.113.1:1080", Enabled: true},
		{ID: "sg-1", Name: "SG", URL: "socks5://user:secret@203.0.113.2:1080", Enabled: true},
		{ID: "off-1", Name: "Off", URL: "socks5://203.0.113.3:1080", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := usage.ReplaceProxyPoolForTenant(sharingTenantA, []config.ProxyPoolEntry{
		{ID: "sg-1", Name: "A's SG", URL: "http://198.51.100.1:3128", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := usage.ReplaceProxyPoolForTenant(sharingTenantB, []config.ProxyPoolEntry{
		{ID: "sg-1", Name: "B's SG", URL: "http://198.51.100.2:3128", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProxySharingIsReservedForPlatformAdminsInTheSystemTenant(t *testing.T) {
	h := setupSharingTest(t)
	seedSharingProxies(t)
	params := gin.Params{{Key: "id", Value: "hk-1"}}

	for _, caller := range []sharingCaller{
		{tenantID: identity.SystemTenantID},             // system tenant, not a platform admin
		{tenantID: sharingTenantA, platformAdmin: true}, // platform admin acting in another tenant
		tenantAAdmin, // tenant admin
	} {
		w := serveSharing(t, caller, http.MethodPut, "/proxy-pool/hk-1/sharing", `{"all":true}`, params, h.PutProxyPoolSharing)
		if w.Code != http.StatusForbidden {
			t.Fatalf("caller %+v: status = %d, body = %s", caller, w.Code, w.Body.String())
		}
	}
	if grants, _ := usage.ListResourceGrants(context.Background(), resourcegrants.KindProxy); len(grants) != 0 {
		t.Fatalf("a refused request still wrote grants: %v", grants)
	}
}

func TestProxySharingValidatesProxyAndTenants(t *testing.T) {
	h := setupSharingTest(t)
	seedSharingProxies(t)

	if w := serveSharing(t, platformAdmin, http.MethodPut, "/proxy-pool/missing/sharing", `{"all":true}`,
		gin.Params{{Key: "id", Value: "missing"}}, h.PutProxyPoolSharing); w.Code != http.StatusNotFound {
		t.Fatalf("unknown proxy: status = %d", w.Code)
	}
	params := gin.Params{{Key: "id", Value: "hk-1"}}
	for _, body := range []string{
		`{"tenant_ids":["33333333-3333-3333-3333-333333333333"]}`,
		`{"tenant_ids":["` + identity.SystemTenantID + `"]}`,
	} {
		if w := serveSharing(t, platformAdmin, http.MethodPut, "/proxy-pool/hk-1/sharing", body, params, h.PutProxyPoolSharing); w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, body = %s", body, w.Code, w.Body.String())
		}
	}
}

func TestSharedProxiesAppearMaskedForGranteesAndAnnotatedForThePlatform(t *testing.T) {
	h := setupSharingTest(t)
	seedSharingProxies(t)

	for id, body := range map[string]string{
		"hk-1": `{"all":true}`,
		"sg-1": `{"tenant_ids":["` + sharingTenantA + `"]}`,
	} {
		w := serveSharing(t, platformAdmin, http.MethodPut, "/proxy-pool/"+id+"/sharing", body, gin.Params{{Key: "id", Value: id}}, h.PutProxyPoolSharing)
		if w.Code != http.StatusOK {
			t.Fatalf("share %s: status = %d, body = %s", id, w.Code, w.Body.String())
		}
	}

	// Tenant A: its own sg-1 stays in items; the shared entries come apart,
	// masked, and sg-1 is marked as overridden.
	w := serveSharing(t, tenantAAdmin, http.MethodGet, "/proxy-pool", "", nil, h.GetProxyPool)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant A GET: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("a grantee saw the platform proxy credentials: %s", w.Body.String())
	}
	var tenantView struct {
		Items       []struct{ ID string } `json:"items"`
		SharedItems []struct {
			ID         string `json:"id"`
			MaskedURL  string `json:"masked_url"`
			Overridden bool   `json:"overridden"`
		} `json:"shared_items"`
		Sharing sharingCapability `json:"sharing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tenantView); err != nil {
		t.Fatal(err)
	}
	if len(tenantView.Items) != 1 || tenantView.Items[0].ID != "sg-1" {
		t.Fatalf("tenant A items = %+v, want only its own sg-1", tenantView.Items)
	}
	if len(tenantView.SharedItems) != 2 {
		t.Fatalf("tenant A shared items = %+v", tenantView.SharedItems)
	}
	for _, item := range tenantView.SharedItems {
		switch item.ID {
		case "hk-1":
			if item.Overridden || item.MaskedURL != "socks5://203.0.113.1:1080" {
				t.Fatalf("hk-1 = %+v", item)
			}
		case "sg-1":
			if !item.Overridden {
				t.Fatalf("sg-1 must show as overridden by tenant A's own entry: %+v", item)
			}
		default:
			t.Fatalf("unexpected shared item %+v", item)
		}
	}
	if !tenantView.Sharing.Supported || tenantView.Sharing.Manageable {
		t.Fatalf("tenant capability = %+v", tenantView.Sharing)
	}

	// Tenant B only got hk-1.
	w = serveSharing(t, tenantBAdmin, http.MethodGet, "/proxy-pool", "", nil, h.GetProxyPool)
	var tenantBView struct {
		SharedItems []struct{ ID string } `json:"shared_items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tenantBView)
	if len(tenantBView.SharedItems) != 1 || tenantBView.SharedItems[0].ID != "hk-1" {
		t.Fatalf("tenant B shared items = %+v", tenantBView.SharedItems)
	}

	// The platform sees each entry's grants and which grantees override it.
	// Tenant B also has an sg-1, but sg-1 is not shared with it.
	w = serveSharing(t, platformAdmin, http.MethodGet, "/proxy-pool", "", nil, h.GetProxyPool)
	var platformView struct {
		Items []struct {
			ID           string           `json:"id"`
			Sharing      *resourceSharing `json:"sharing"`
			OverriddenBy []tenantRef      `json:"overridden_by"`
		} `json:"items"`
		Sharing sharingCapability `json:"sharing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &platformView); err != nil {
		t.Fatal(err)
	}
	if !platformView.Sharing.Manageable {
		t.Fatalf("platform capability = %+v", platformView.Sharing)
	}
	byID := map[string]int{}
	for i, item := range platformView.Items {
		byID[item.ID] = i
	}
	hk := platformView.Items[byID["hk-1"]]
	if hk.Sharing == nil || !hk.Sharing.All || len(hk.OverriddenBy) != 0 {
		t.Fatalf("hk-1 = %+v", hk)
	}
	sg := platformView.Items[byID["sg-1"]]
	if sg.Sharing == nil || !slices.Equal(sg.Sharing.TenantIDs, []string{sharingTenantA}) {
		t.Fatalf("sg-1 sharing = %+v", sg.Sharing)
	}
	if len(sg.OverriddenBy) != 1 || sg.OverriddenBy[0].ID != sharingTenantA || sg.OverriddenBy[0].Name != "Tenant A" {
		t.Fatalf("sg-1 overridden_by = %+v", sg.OverriddenBy)
	}
	if off := platformView.Items[byID["off-1"]]; off.Sharing != nil {
		t.Fatalf("unshared entry shows sharing: %+v", off)
	}
}

func TestProxyAdoptRemovesOnlyGranteeOverrides(t *testing.T) {
	h := setupSharingTest(t)
	seedSharingProxies(t)
	if _, err := usage.ReplaceResourceGrants(context.Background(), resourcegrants.KindProxy, []string{"sg-1"}, []string{sharingTenantA}); err != nil {
		t.Fatal(err)
	}

	w := serveSharing(t, platformAdmin, http.MethodPost, "/proxy-pool/sg-1/adopt", "", gin.Params{{Key: "id", Value: "sg-1"}}, h.PostProxyPoolAdopt)
	if w.Code != http.StatusOK {
		t.Fatalf("adopt: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Tenants []tenantRef `json:"tenants"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Tenants) != 1 || resp.Tenants[0].ID != sharingTenantA {
		t.Fatalf("adopted tenants = %+v", resp.Tenants)
	}
	if own := usage.ListProxyPoolForTenant(sharingTenantA); len(own) != 0 {
		t.Fatalf("tenant A still overrides sg-1: %+v", own)
	}
	// Not shared with B: removing B's entry would strand its accounts.
	if own := usage.ListProxyPoolForTenant(sharingTenantB); len(own) != 1 {
		t.Fatalf("tenant B lost its own sg-1: %+v", own)
	}
	tenantCfg := usage.BuildTenantRuntimeConfig(&config.Config{}, sharingTenantA)
	if got := tenantCfg.ResolveProxyURL("sg-1", ""); got != "socks5://user:secret@203.0.113.2:1080" {
		t.Fatalf("tenant A now resolves sg-1 to %q, want the shared entry", got)
	}

	// A disabled shared entry would leave the accounts without a proxy.
	if _, err := usage.ReplaceResourceGrants(context.Background(), resourcegrants.KindProxy, []string{"off-1"}, []string{resourcegrants.AllTenants}); err != nil {
		t.Fatal(err)
	}
	w = serveSharing(t, platformAdmin, http.MethodPost, "/proxy-pool/off-1/adopt", "", gin.Params{{Key: "id", Value: "off-1"}}, h.PostProxyPoolAdopt)
	if w.Code != http.StatusConflict {
		t.Fatalf("adopting a disabled proxy: %d %s", w.Code, w.Body.String())
	}
}

func registerSharingAuth(t *testing.T, manager *coreauth.Manager, id, tenantID string) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:         id,
		TenantID:   tenantID,
		Provider:   "codex",
		Label:      "acct-" + id,
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": filepath.Join(t.TempDir(), id)},
		Metadata:   map[string]any{"email": id + "@example.com", "access_token": "secret-token"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestAuthFileSharingGrantsSystemCredentialsReadOnly(t *testing.T) {
	h := setupSharingTest(t)
	registerSharingAuth(t, h.authManager, "codex-platform.json", identity.SystemTenantID)
	registerSharingAuth(t, h.authManager, sharingTenantA+"/codex-a.json", sharingTenantA)

	// Only system credentials can be shared.
	w := serveSharing(t, platformAdmin, http.MethodPut, "/auth-files/sharing",
		`{"ids":["`+sharingTenantA+`/codex-a.json"],"all":true}`, nil, h.PutAuthFileSharing)
	if w.Code != http.StatusNotFound {
		t.Fatalf("sharing another tenant's credential: %d %s", w.Code, w.Body.String())
	}
	w = serveSharing(t, tenantAAdmin, http.MethodPut, "/auth-files/sharing",
		`{"ids":["codex-platform.json"],"all":true}`, nil, h.PutAuthFileSharing)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant admin changed sharing: %d", w.Code)
	}

	w = serveSharing(t, platformAdmin, http.MethodPut, "/auth-files/sharing",
		`{"ids":["codex-platform.json"],"tenant_ids":["`+sharingTenantA+`"]}`, nil, h.PutAuthFileSharing)
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	// Applied on this node right away.
	if shared := h.authManager.ListSharedWithTenant(sharingTenantA); len(shared) != 1 || shared[0].ID != "codex-platform.json" {
		t.Fatalf("manager shared with A = %v", shared)
	}
	if shared := h.authManager.ListSharedWithTenant(sharingTenantB); len(shared) != 0 {
		t.Fatalf("tenant B was not granted anything: %v", shared)
	}

	// Tenant A manages only its own credential and sees the shared one apart,
	// without the owner's file path or token.
	w = serveSharing(t, tenantAAdmin, http.MethodGet, "/auth-files", "", nil, h.ListAuthFiles)
	if strings.Contains(w.Body.String(), "secret-token") {
		t.Fatalf("a grantee saw a token: %s", w.Body.String())
	}
	var tenantView struct {
		Files       []map[string]any `json:"files"`
		SharedFiles []map[string]any `json:"shared_files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tenantView); err != nil {
		t.Fatal(err)
	}
	if len(tenantView.Files) != 1 || tenantView.Files[0]["id"] != sharingTenantA+"/codex-a.json" {
		t.Fatalf("tenant A files = %v", tenantView.Files)
	}
	if len(tenantView.SharedFiles) != 1 {
		t.Fatalf("tenant A shared files = %v", tenantView.SharedFiles)
	}
	shared := tenantView.SharedFiles[0]
	// The tenant sees the platform's label, not who the account is: the
	// credential id becomes a stand-in and the login email is dropped.
	if id, _ := shared["id"].(string); !strings.HasPrefix(id, "shared-") || shared["shared"] != true || shared["label"] != "acct-codex-platform.json" {
		t.Fatalf("shared entry = %v", shared)
	}
	if _, leaked := shared["email"]; leaked {
		t.Fatalf("shared entry carries the account's email: %v", shared)
	}
	if encoded, _ := json.Marshal(shared); strings.Contains(string(encoded), "@example.com") {
		t.Fatalf("shared entry names the account's address: %s", encoded)
	}
	if _, leaked := shared["path"]; leaked {
		t.Fatalf("shared entry carries the owner's file path: %v", shared)
	}

	// The platform sees who each credential is shared with.
	w = serveSharing(t, platformAdmin, http.MethodGet, "/auth-files", "", nil, h.ListAuthFiles)
	var platformView struct {
		Files []struct {
			ID      string           `json:"id"`
			Sharing *resourceSharing `json:"sharing"`
		} `json:"files"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &platformView)
	if len(platformView.Files) != 1 || platformView.Files[0].Sharing == nil ||
		!slices.Equal(platformView.Files[0].Sharing.TenantIDs, []string{sharingTenantA}) {
		t.Fatalf("platform files = %+v", platformView.Files)
	}

	// Deleting the credential revokes its grants.
	h.dropSharedAuthGrants(context.Background(), identity.SystemTenantID, []string{"codex-platform.json"})
	if grants, _ := usage.ListResourceGrants(context.Background(), resourcegrants.KindAuth); len(grants) != 0 {
		t.Fatalf("grants outlived the credential: %v", grants)
	}
	if shared := h.authManager.ListSharedWithTenant(sharingTenantA); len(shared) != 0 {
		t.Fatalf("manager still shares the deleted credential: %v", shared)
	}
}

func TestAuthFilesFlagCopiesOfSharedAccounts(t *testing.T) {
	h := setupSharingTest(t)
	register := func(id, tenantID string) {
		t.Helper()
		auth := &coreauth.Auth{
			ID:         id,
			TenantID:   tenantID,
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"path": filepath.Join(t.TempDir(), "creds.json")},
			// Same upstream account in both tenants: the case that makes their
			// refresh tokens revoke each other.
			Metadata: map[string]any{"account_id": "acct-shared-1", "email": "ops@example.com"},
		}
		if _, err := h.authManager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	register("codex-platform.json", identity.SystemTenantID)
	register(sharingTenantA+"/codex-copy.json", sharingTenantA)
	if _, err := usage.ReplaceResourceGrants(context.Background(), resourcegrants.KindAuth, []string{"codex-platform.json"}, []string{sharingTenantA}); err != nil {
		t.Fatal(err)
	}
	h.reloadSharedAuthGrants()

	w := serveSharing(t, platformAdmin, http.MethodGet, "/auth-files", "", nil, h.ListAuthFiles)
	var platformView struct {
		Files []struct {
			ID           string      `json:"id"`
			TenantCopies []tenantRef `json:"tenant_copies"`
		} `json:"files"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &platformView)
	if len(platformView.Files) != 1 || len(platformView.Files[0].TenantCopies) != 1 || platformView.Files[0].TenantCopies[0].ID != sharingTenantA {
		t.Fatalf("platform view = %+v", platformView.Files)
	}

	w = serveSharing(t, tenantAAdmin, http.MethodGet, "/auth-files", "", nil, h.ListAuthFiles)
	var tenantView struct {
		Files []map[string]any `json:"files"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tenantView)
	if len(tenantView.Files) != 1 || tenantView.Files[0]["duplicates_shared"] != true {
		t.Fatalf("tenant A's copy is not flagged: %+v", tenantView.Files)
	}
}
