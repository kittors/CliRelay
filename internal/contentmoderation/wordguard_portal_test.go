package contentmoderation

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// runWordPortalChecks holds the portal users' policies and permissions to the
// same behaviour on SQLite and on PostgreSQL: the read that serves requests
// fetches them with a UNION of two tables, which each database must accept.
func runWordPortalChecks(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("everyone is allowed until the tenant decides otherwise", func(t *testing.T) {
		access, err := store.GetWordPortalAccess(ctx, "tenant-p0")
		if err != nil || !access.DefaultAllowed || len(access.Overrides) != 0 || !access.Allowed("u-1") {
			t.Fatalf("access = %+v, %v", access, err)
		}
		if allowed, err := store.WordPortalAllowed(ctx, "tenant-p0", "u-1"); err != nil || !allowed {
			t.Fatalf("allowed = %v, %v", allowed, err)
		}
		// A tenant that closes it closes it for everyone without an override.
		if err := store.SetWordPortalDefault(ctx, "tenant-p0", false); err != nil {
			t.Fatal(err)
		}
		if allowed, err := store.WordPortalAllowed(ctx, "tenant-p0", "u-1"); err != nil || allowed {
			t.Fatalf("after closing: allowed = %v, %v", allowed, err)
		}
	})

	t.Run("an override beats the default either way", func(t *testing.T) {
		const tenant = "tenant-p1"
		if err := store.SetWordPortalDefault(ctx, tenant, true); err != nil {
			t.Fatal(err)
		}
		deny, allow := false, true
		if err := store.SetWordPortalOverride(ctx, tenant, "u-deny", &deny); err != nil {
			t.Fatal(err)
		}
		access, err := store.GetWordPortalAccess(ctx, tenant)
		if err != nil || !access.Allowed("u-any") || access.Allowed("u-deny") {
			t.Fatalf("access = %+v, %v", access, err)
		}
		if err := store.SetWordPortalDefault(ctx, tenant, false); err != nil {
			t.Fatal(err)
		}
		if err := store.SetWordPortalOverride(ctx, tenant, "u-vip", &allow); err != nil {
			t.Fatal(err)
		}
		for user, want := range map[string]bool{"u-any": false, "u-deny": false, "u-vip": true} {
			if got, err := store.WordPortalAllowed(ctx, tenant, user); err != nil || got != want {
				t.Fatalf("%s allowed = %v (%v), want %v", user, got, err, want)
			}
		}
		// Clearing the override hands the user back to the default.
		if err := store.SetWordPortalOverride(ctx, tenant, "u-vip", nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.WordPortalAllowed(ctx, tenant, "u-vip"); got {
			t.Fatal("a cleared override still allows")
		}
		if err := store.SetWordPortalOverride(ctx, tenant, " ", &allow); err == nil {
			t.Fatal("an override without a user was stored")
		}
	})

	t.Run("a request applies the user's policy only while it is allowed", func(t *testing.T) {
		const tenant = "tenant-p2"
		savedWordPolicy(t, store, tenant, WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("tenantword")}}, nil)
		savedWordPolicy(t, store, tenant, WordPolicy{ScopeType: WordScopeEndUser, ScopeID: "u-1", Enabled: true, Rules: []WordRule{kw("myword")}}, nil)

		// Nothing decided: the user's words apply.
		got, err := store.LoadWordPolicies(ctx, tenant, "k-1", "u-1")
		if err != nil || got.Tenant == nil || got.EndUser == nil || got.EndUser.Rules[0].Pattern != "myword" {
			t.Fatalf("allowed by default: %+v, %v", got, err)
		}
		// Closed for the tenant, then opened again.
		if err := store.SetWordPortalDefault(ctx, tenant, false); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.LoadWordPolicies(ctx, tenant, "k-1", "u-1"); got.EndUser != nil {
			t.Fatalf("closed for the tenant, the user's words still apply: %+v", got.EndUser)
		}
		if err := store.SetWordPortalDefault(ctx, tenant, true); err != nil {
			t.Fatal(err)
		}
		got, err = store.LoadWordPolicies(ctx, tenant, "k-1", "u-1")
		if err != nil || got.EndUser == nil || got.EndUser.Rules[0].Pattern != "myword" {
			t.Fatalf("allowed: %+v, %v", got, err)
		}
		// Another user's policy is never the one read, nor is a key no portal user
		// owns given one.
		if got, _ := store.LoadWordPolicies(ctx, tenant, "k-2", "u-2"); got.EndUser != nil {
			t.Fatalf("u-2 got u-1's policy: %+v", got.EndUser)
		}
		if got, _ := store.LoadWordPolicies(ctx, tenant, "k-3", ""); got.EndUser != nil || got.Tenant == nil {
			t.Fatalf("a key of no portal user: %+v", got)
		}
		// Taken away: it stops applying at once, and it is kept.
		deny := false
		if err := store.SetWordPortalOverride(ctx, tenant, "u-1", &deny); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.LoadWordPolicies(ctx, tenant, "k-1", "u-1"); got.EndUser != nil {
			t.Fatal("a revoked user's policy still applies")
		}
		if store.rememberedWordPolicies(tenant, "k-1", "u-1").EndUser != nil {
			t.Fatal("the fallback for a slow database still applies a revoked user's policy")
		}
		if _, err := store.GetWordPolicy(ctx, tenant, WordScopeEndUser, "u-1"); err != nil {
			t.Fatalf("the revoked user's policy was not kept: %v", err)
		}
		if err := store.SetWordPortalOverride(ctx, tenant, "u-1", nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.LoadWordPolicies(ctx, tenant, "k-1", "u-1"); got.EndUser == nil {
			t.Fatal("allowed again, the kept policy does not apply")
		}
		if store.rememberedWordPolicies(tenant, "k-1", "u-1").EndUser == nil {
			t.Fatal("the fallback forgot the policy of an allowed user")
		}
	})

	t.Run("summaries list the portal users' policies", func(t *testing.T) {
		const tenant = "tenant-p3"
		savedWordPolicy(t, store, tenant, WordPolicy{ScopeType: WordScopeEndUser, ScopeID: "u-9", Enabled: true, Rules: []WordRule{kw("a"), kw("b")}}, nil)
		summaries, err := store.ListWordPolicySummaries(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, summary := range summaries {
			if summary.ScopeType == WordScopeEndUser && summary.ScopeID == "u-9" && summary.RuleCount == 2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("summaries = %+v", summaries)
		}
	})

	t.Run("deleting a user removes the policy and the override", func(t *testing.T) {
		const tenant = "tenant-p4"
		allow := true
		savedWordPolicy(t, store, tenant, WordPolicy{ScopeType: WordScopeEndUser, ScopeID: "u-gone", Enabled: true, Rules: []WordRule{kw("x")}}, nil)
		if err := store.SetWordPortalOverride(ctx, tenant, "u-gone", &allow); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteEndUserWordData(ctx, tenant, "u-gone"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetWordPolicy(ctx, tenant, WordScopeEndUser, "u-gone"); !errors.Is(err, ErrWordPolicyNotFound) {
			t.Fatalf("policy left behind: %v", err)
		}
		if access, _ := store.GetWordPortalAccess(ctx, tenant); len(access.Overrides) != 0 {
			t.Fatalf("override left behind: %+v", access)
		}
	})
}

func TestWordPortalStoreSQLite(t *testing.T) {
	runWordPortalChecks(t, newTestStore(t))
}

func TestWordPortalStorePostgres(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db := scopedPostgres(t, dsn, "cm_words_portal")
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	runWordPortalChecks(t, NewStore(db))
}

func TestEndUserPolicyNeedsItsUserAndKeepsToItsLimits(t *testing.T) {
	missing := WordPolicy{ScopeType: WordScopeEndUser, Enabled: true}
	var invalid *WordPolicyError
	if err := missing.Normalize(); !errors.As(err, &invalid) || invalid.Field != "scope_id" {
		t.Fatalf("a portal user policy without its user: %v", err)
	}

	tooMany := WordPolicy{ScopeType: WordScopeEndUser, ScopeID: "u-1", Enabled: true}
	for i := 0; i <= MaxWordRulesPerEndUserPolicy; i++ {
		tooMany.Rules = append(tooMany.Rules, kw("word"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune(0x4e00+i))))
	}
	if err := tooMany.Normalize(); !errors.As(err, &invalid) || len(invalid.Rules) == 0 || invalid.Rules[0].Code != "too_many_rules" {
		t.Fatalf("%d rules in a portal user policy: %v", len(tooMany.Rules), err)
	}
	// The same list is fine for a tenant.
	tenant := WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: tooMany.Rules}
	if err := tenant.Normalize(); err != nil {
		t.Fatalf("tenant: %v", err)
	}

	regexes := WordPolicy{ScopeType: WordScopeEndUser, ScopeID: "u-1", Enabled: true}
	for i := 0; i <= MaxWordRegexPerEndUserPolicy; i++ {
		regexes.Rules = append(regexes.Rules, rx(`a\d{`+string(rune('1'+i%9))+`}`+strings.Repeat("b", i)))
	}
	if err := regexes.Normalize(); !errors.As(err, &invalid) || invalid.Rules[len(invalid.Rules)-1].Code != "too_many_regex" {
		t.Fatalf("regular expressions: %v", err)
	}
	if limits := WordLimitsFor(WordScopeEndUser); limits.MaxRules != MaxWordRulesPerEndUserPolicy || limits.MaxRegex != MaxWordRegexPerEndUserPolicy {
		t.Fatalf("limits = %+v", limits)
	}
}

// A portal user's own policy can add a block, but nothing in it changes what
// the tenant decided: the tenant only logs a word the user blocks, and a word the
// tenant blocks stays blocked whatever the user sets.
func TestEndUserPolicyOnlyAddsToTheTenant(t *testing.T) {
	tenant := tenantOf(t, WordActionLog, kw("tenantword"))
	endUser := policyOf(t, WordScopeEndUser, "u-1", func(p *WordPolicy) { p.Action = WordActionBlock; p.BlockMessage = "mine" }, kw("userword"))
	contexts := buildPolicyContexts(WordPolicies{Tenant: tenant, EndUser: endUser})
	if len(contexts) != 2 || contexts[1].layer != WordScopeEndUser {
		t.Fatalf("contexts = %+v", contexts)
	}
	verdict := evalOne(t, sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"a userword here"}]}`, contexts)
	if !verdict.Blocked || verdict.BlockMessage != "mine" || verdict.Matches[0].Layer != WordScopeEndUser {
		t.Fatalf("the user's block: %+v", verdict)
	}

	strict := tenantOf(t, WordActionBlock, kw("tenantword"))
	lenient := policyOf(t, WordScopeEndUser, "u-1", func(p *WordPolicy) { p.Action = WordActionLog; p.ScanScope = WordScanLastUser }, kw("other"))
	verdict = evalOne(t, sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"tenantword"},{"role":"user","content":"fine"}]}`, buildPolicyContexts(WordPolicies{Tenant: strict, EndUser: lenient}))
	if !verdict.Blocked || verdict.Matches[0].Layer != WordScopeTenant {
		t.Fatalf("the user's settings weakened the tenant's: %+v", verdict)
	}

	// The cache builds the same contexts, and a disabled user policy adds none.
	var cache wordContextCache
	if got := cache.policyContexts(WordPolicies{Tenant: tenant, EndUser: endUser}); len(got) != 2 || got[1].layer != WordScopeEndUser {
		t.Fatalf("cached contexts = %+v", got)
	}
	off := policyOf(t, WordScopeEndUser, "u-1", func(p *WordPolicy) { p.Enabled = false }, kw("userword"))
	if got := cache.policyContexts(WordPolicies{EndUser: off}); len(got) != 0 {
		t.Fatalf("a disabled user policy made a context: %+v", got)
	}
}

func TestGuardRequestLoadsThePolicyOfTheKeysPortalUser(t *testing.T) {
	ctx, ginCtx := snapshotContext(t)
	ginCtx.Set("accessMetadata", map[string]string{"api-key-id": "key-7", "end-user-id": " user-3 ", "tenant-id": "tenant-a"})
	endUser := policyOf(t, WordScopeEndUser, "user-3", nil, kw("forbidden"))
	resolver := &wordResolverStub{policies: WordPolicies{EndUser: endUser}}
	req, opts := guardOptions("tenant-a", chatRequest("a forbidden word"))
	result := guardModerator(resolver).GuardRequest(ctx, req, opts)
	if len(resolver.endUsers) != 1 || resolver.endUsers[0] != "user-3" {
		t.Fatalf("end users = %q", resolver.endUsers)
	}
	if !result.Blocked {
		t.Fatalf("the portal user's policy was not applied: %+v", result)
	}
	snapshot, ok := WordGuardSnapshotFromGin(ginCtx)
	if !ok {
		t.Fatal("no snapshot")
	}
	raw, _ := snapshot.MarshalJSON()
	if !strings.Contains(string(raw), `"end_user"`) {
		t.Fatalf("the request log does not name the layer: %s", raw)
	}
}

func TestDryRunAppliesThePortalUsersPolicy(t *testing.T) {
	endUser := policyOf(t, WordScopeEndUser, "u-1", func(p *WordPolicy) { p.Action = WordActionReplace; p.Replacement = "##" }, kw("secret"))
	result := DryRunWordPolicies(WordPolicies{EndUser: endUser}, "my secret plan", false)
	if result.Output != "my ## plan" || len(result.Layers) != 1 || result.Layers[0] != WordScopeEndUser || result.Matches[0].Pattern != "secret" {
		t.Fatalf("dry run = %+v", result)
	}
}
