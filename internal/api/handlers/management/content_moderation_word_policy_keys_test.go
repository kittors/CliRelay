package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

// callWithQuery runs a handler as a caller of the given tenant, with a query
// string (which the shared call helper cannot carry).
func callWithQuery(t *testing.T, handler gin.HandlerFunc, caller wordCaller, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{PlatformAdmin: caller.admin, EffectiveTenant: identity.Tenant{ID: caller.tenant}})
	c.Request = httptest.NewRequest(method, target, nil)
	handler(c)
	c.Writer.WriteHeaderNow()
	return rec
}

func keyPolicyExists(t *testing.T, h *Handler, tenantID, keyID string) bool {
	t.Helper()
	_, err := h.contentModerationStore().GetWordPolicy(context.Background(), tenantID, contentmoderation.WordScopeKey, keyID)
	if errors.Is(err, contentmoderation.ErrWordPolicyNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("GetWordPolicy(%s): %v", keyID, err)
	}
	return true
}

// The override of a key is the key's own: when the key is deleted it goes, and
// a key that is created again under the same id (an import, a restore) starts
// without it.
func TestDeletingAKeyDeletesItsWordPolicy(t *testing.T) {
	h := setupWordPolicyTest(t)
	for id, name := range map[string]string{"key-1": "One", "key-2": "Two", "key-3": "Three"} {
		createWordPolicyKey(t, wordTenantA, id, name)
	}
	createWordPolicyKey(t, wordTenantB, "key-b", "Other tenant")
	for id, caller := range map[string]wordCaller{"key-1": wordTenantAUser, "key-2": wordTenantAUser, "key-3": wordTenantAUser, "key-b": wordTenantBUser} {
		rec, _ := caller.call(h.PutWordPolicyKey, http.MethodPut, `{"rules":[{"pattern":"override-`+id+`"}]}`, id)
		wordExpectStatus(t, rec, http.StatusOK)
	}

	// The entry route, by id.
	rec := callWithQuery(t, h.DeleteAPIKeyEntry, wordTenantAUser, http.MethodDelete, "/v0/management/api-key-entries?id=key-1")
	wordExpectStatus(t, rec, http.StatusOK)
	if keyPolicyExists(t, h, wordTenantA, "key-1") {
		t.Fatal("the override of the deleted key is still stored")
	}
	// The legacy route, by the secret.
	rec = callWithQuery(t, h.DeleteAPIKeys, wordTenantAUser, http.MethodDelete, "/v0/management/api-keys?value=sk-key-2")
	wordExpectStatus(t, rec, http.StatusOK)
	if keyPolicyExists(t, h, wordTenantA, "key-2") {
		t.Fatal("the override of the key deleted by its secret is still stored")
	}

	// What belongs to the other keys is left alone, in this tenant and in another.
	if !keyPolicyExists(t, h, wordTenantA, "key-3") || !keyPolicyExists(t, h, wordTenantB, "key-b") {
		t.Fatal("deleting a key took the override of another key with it")
	}

	// A key created again under an old id has nothing to inherit.
	createWordPolicyKey(t, wordTenantA, "key-1", "One again")
	rec, got := wordTenantAUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-1")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["exists"] != false {
		t.Fatalf("the new key inherited the old override: %v", got)
	}
}

func TestDeleteWordPoliciesForKeys(t *testing.T) {
	h := setupWordPolicyTest(t)
	store := h.contentModerationStore()
	ctx := context.Background()
	save := func(tenantID, scope, scopeID string) {
		policy := contentmoderation.WordPolicy{TenantID: tenantID, ScopeType: scope, ScopeID: scopeID, Enabled: true, Rules: []contentmoderation.WordRule{{Kind: contentmoderation.WordRuleKeyword, Pattern: "x", Enabled: true}}}
		if err := policy.Normalize(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutWordPolicy(ctx, policy, nil); err != nil {
			t.Fatal(err)
		}
	}
	save(wordTenantA, contentmoderation.WordScopeTenant, "")
	for i := 0; i < 450; i++ { // more keys than one statement carries
		save(wordTenantA, contentmoderation.WordScopeKey, "k"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)))
	}
	save(wordTenantB, contentmoderation.WordScopeKey, "kaaa")

	ids := []string{" ", ""}
	for i := 0; i < 450; i++ {
		ids = append(ids, "k"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)))
	}
	ids = append(ids, "never-had-one")
	removed, err := store.DeleteWordPoliciesForKeys(ctx, wordTenantA, ids)
	if err != nil || removed != 450 {
		t.Fatalf("removed = %d, err = %v, want 450", removed, err)
	}
	if _, err := store.GetWordPolicy(ctx, wordTenantA, contentmoderation.WordScopeTenant, ""); err != nil {
		t.Fatalf("the tenant's own policy must stay: %v", err)
	}
	if _, err := store.GetWordPolicy(ctx, wordTenantB, contentmoderation.WordScopeKey, "kaaa"); err != nil {
		t.Fatalf("another tenant's key policy must stay: %v", err)
	}
	if removed, err := store.DeleteWordPoliciesForKeys(ctx, wordTenantA, nil); err != nil || removed != 0 {
		t.Fatalf("nothing to delete: %d, %v", removed, err)
	}
	if _, err := (*contentmoderation.Store)(nil).DeleteWordPoliciesForKeys(ctx, wordTenantA, ids); !errors.Is(err, contentmoderation.ErrUnavailable) {
		t.Fatalf("without a store: %v", err)
	}
}
