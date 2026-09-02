package usage

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func resetMergedLegacySubjects() {
	mergedLegacySubjects.Range(func(key, _ any) bool {
		mergedLegacySubjects.Delete(key)
		return true
	})
}

func seedLegacyAuthSubjectRow(t *testing.T, legacyID, provider, seedKind string, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339Nano)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subjects (
			auth_subject_id, provider, subject_scope, seed_kind, seed_hash, share_eligible,
			usage_projected_since, usage_history_complete, created_at, updated_at
		) VALUES (?, ?, 'tenant', ?, ?, 0, ?, 0, ?, ?)
	`, legacyID, provider, seedKind, "hash-"+legacyID, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func seedLegacySubjectStatus(t *testing.T, legacyID, provider, planType string, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339Nano)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_status (
			auth_subject_id, provider, last_probe_state, health_status, plan_type, updated_at
		) VALUES (?, ?, 'success', 'ok', ?, ?)
	`, legacyID, provider, planType, stamp); err != nil {
		t.Fatal(err)
	}
}

func readSubjectSummary(t *testing.T, subjectID string) AuthSubjectUsageSummary {
	t.Helper()
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{subjectID}, map[string]time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return summaries[subjectID]
}

// Production: one Google account mounted by three tenants became three subjects.
// The tenant that had not called it yet showed 0 calls / 0 tokens next to a quota
// bar reading 87%, because the account's usage was filed under another tenant's
// copy of the same account.
func TestMergeLegacySplitSubjectFoldsTenantHalvesOntoOneAccount(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "a.json", "", "shared@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "b.json", "", "shared@example.com")
	authA.Provider, authB.Provider = "antigravity", "antigravity"
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if identityA.ID != identityB.ID {
		t.Fatalf("one account still split: %q != %q", identityA.ID, identityB.ID)
	}
	legacyA := LegacyTenantScopedAuthSubjectID(authA)
	legacyB := LegacyTenantScopedAuthSubjectID(authB)
	if legacyA == "" || legacyA == legacyB || legacyA == identityA.ID {
		t.Fatalf("legacy ids look wrong: A=%q B=%q shared=%q", legacyA, legacyB, identityA.ID)
	}

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyA, "antigravity", "email", now.Add(-72*time.Hour))
	seedLegacyAuthSubjectRow(t, legacyB, "antigravity", "email", now.Add(-24*time.Hour))
	// The half probed most recently is the one whose plan must survive.
	seedLegacySubjectStatus(t, legacyA, "antigravity", "stale-plan", now.Add(-10*time.Hour))
	seedLegacySubjectStatus(t, legacyB, "antigravity", "google-ai-pro", now.Add(-time.Hour))

	for i := 0; i < 49; i++ {
		projectSubjectRequest(t, legacyA, now.Add(-48*time.Hour), 100)
	}
	for i := 0; i < 3; i++ {
		projectSubjectRequest(t, legacyB, now.Add(-2*time.Hour), 10)
	}
	if _, err := getDB().Exec(`
		INSERT INTO request_logs (tenant_id, timestamp, auth_subject_id, model, failed, streaming)
		VALUES (?, ?, ?, 'gemini-pro', 0, 0)
	`, sharedSubjectTenantA, now.Add(-48*time.Hour).Format(time.RFC3339Nano), legacyA); err != nil {
		t.Fatal(err)
	}

	if err := MergeLegacySplitAuthSubject(authA, identityA); err != nil {
		t.Fatal(err)
	}
	if err := MergeLegacySplitAuthSubject(authB, identityB); err != nil {
		t.Fatal(err)
	}

	got := readSubjectSummary(t, identityA.ID)
	if got.RequestTotal != 52 || got.SuccessTotal != 52 || got.CostTotal == 0 {
		t.Fatalf("merged lifetime = %+v, want 52 requests", got)
	}

	var planType string
	if err := getDB().QueryRow(
		`SELECT plan_type FROM ai_account_subject_status WHERE auth_subject_id = ?`, identityA.ID,
	).Scan(&planType); err != nil {
		t.Fatal(err)
	}
	if planType != "google-ai-pro" {
		t.Fatalf("status plan = %q, want the most recently probed half", planType)
	}

	for _, legacyID := range []string{legacyA, legacyB} {
		var leftovers int
		if err := getDB().QueryRow(`
			SELECT (SELECT COUNT(*) FROM ai_account_subjects WHERE auth_subject_id = ?)
			     + (SELECT COUNT(*) FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ?)
			     + (SELECT COUNT(*) FROM ai_account_subject_status WHERE auth_subject_id = ?)
			     + (SELECT COUNT(*) FROM request_logs WHERE auth_subject_id = ?)
		`, legacyID, legacyID, legacyID, legacyID).Scan(&leftovers); err != nil {
			t.Fatal(err)
		}
		if leftovers != 0 {
			t.Fatalf("legacy subject %s left %d rows behind", legacyID, leftovers)
		}
	}

	// Idempotent: a reload must not double the totals.
	resetMergedLegacySubjects()
	if err := MergeLegacySplitAuthSubject(authA, identityA); err != nil {
		t.Fatal(err)
	}
	if again := readSubjectSummary(t, identityA.ID); again.RequestTotal != 52 {
		t.Fatalf("re-running the merge changed totals: %+v", again)
	}
}

// The same API key mounted by two tenants is one upstream account. Its id is
// stored tenant-prefixed, which is what used to split it.
func TestMergeLegacySplitSubjectFoldsSharedAPIKey(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	authA := &coreauth.Auth{
		ID: sharedSubjectTenantA + "/codex:apikey:abc123", TenantID: sharedSubjectTenantA,
		Provider: "codex", FileName: "key-a.json",
	}
	authB := &coreauth.Auth{
		ID: sharedSubjectTenantB + "/codex:apikey:abc123", TenantID: sharedSubjectTenantB,
		Provider: "codex", FileName: "key-b.json",
	}
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if identityA.ID != identityB.ID || identityA.SeedKind != "auth_id" {
		t.Fatalf("API key identities = %+v / %+v", identityA, identityB)
	}

	now := time.Now().UTC().Truncate(time.Second)
	for _, seed := range []struct {
		auth     *coreauth.Auth
		identity *AuthSubjectIdentity
		requests int
	}{{authA, identityA, 5}, {authB, identityB, 7}} {
		legacyID := LegacyTenantScopedAuthSubjectID(seed.auth)
		seedLegacyAuthSubjectRow(t, legacyID, "codex", "auth_id", now.Add(-time.Hour))
		for i := 0; i < seed.requests; i++ {
			projectSubjectRequest(t, legacyID, now.Add(-time.Hour), 20)
		}
		if err := MergeLegacySplitAuthSubject(seed.auth, seed.identity); err != nil {
			t.Fatal(err)
		}
	}

	if got := readSubjectSummary(t, identityA.ID); got.RequestTotal != 12 {
		t.Fatalf("merged API key lifetime = %+v, want 12 requests", got)
	}
}

// Merging brings together halves that were anchored independently, which is the
// fragmented-period shape again — the totals must not read zero afterwards.
func TestMergeLegacySplitSubjectRealignsCycleBuckets(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "cyc-a.json", "", "cycle@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "cyc-b.json", "", "cycle@example.com")
	authA.Provider, authB.Provider = "antigravity", "antigravity"
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)

	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(96 * time.Hour)
	percent := 70.0
	for _, seed := range []struct {
		auth     *coreauth.Auth
		identity *AuthSubjectIdentity
		at       time.Time
		requests int
	}{
		{authA, identityA, now.Add(-50 * time.Hour), 9},
		{authB, identityB, now.Add(-20 * time.Hour), 4},
	} {
		legacyID := LegacyTenantScopedAuthSubjectID(seed.auth)
		seedLegacyAuthSubjectRow(t, legacyID, "antigravity", "email", now.Add(-72*time.Hour))
		resetAt := reset
		if err := RecordAIAccountSubjectQuotaPoints(legacyID, "antigravity", []QuotaSnapshotPoint{{
			RecordedAt: seed.at, Provider: "antigravity", QuotaKey: "antigravity:gemini_weekly",
			QuotaLabel: "Gemini", Percent: &percent, ResetAt: &resetAt, WindowSeconds: 604800,
		}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < seed.requests; i++ {
			projectSubjectRequest(t, legacyID, seed.at, 50)
		}
		if err := MergeLegacySplitAuthSubject(seed.auth, seed.identity); err != nil {
			t.Fatal(err)
		}
	}

	if buckets := countCycleBuckets(t, identityA.ID); buckets != 1 {
		t.Fatalf("cycle buckets after merge = %d, want 1", buckets)
	}
	got := readCycleSummary(t, identityA.ID)
	if !got.CycleKnown || got.CycleRequestTotal != 13 {
		t.Fatalf("merged cycle summary = %+v, want 13 requests", got)
	}
}

// The merge has to happen on the auth-load path: it is the only place holding an
// authoritative auth, and it is what an operator's restart actually runs.
func TestBindingHookMergesLegacySubjectOnAuthLoad(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "hook.json", "", "hook@example.com")
	auth.Provider = "antigravity"
	identity := ResolveAuthSubjectIdentity(auth)
	legacyID := LegacyTenantScopedAuthSubjectID(auth)

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyID, "antigravity", "email", now.Add(-time.Hour))
	for i := 0; i < 8; i++ {
		projectSubjectRequest(t, legacyID, now.Add(-time.Hour), 25)
	}

	AIAccountBindingHook{}.OnAuthLoaded(context.Background(), auth)

	if got := readSubjectSummary(t, identity.ID); got.RequestTotal != 8 || got.SuccessTotal != 8 {
		t.Fatalf("hook did not migrate the account: %+v", got)
	}
	bindings, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{auth.ID})
	if err != nil || len(bindings) != 1 || bindings[0].AuthSubjectID != identity.ID {
		t.Fatalf("binding=%+v err=%v, want it pointing at the shared subject", bindings, err)
	}
}

// Fingerprint rows are unique per (tenant, provider, account_key, profile_key).
// A tenant holding both halves collides only within one profile, so the merge
// must compare profiles too — matching on tenant and provider alone discards
// profiles the account still needs.
func TestMergeLegacySplitSubjectKeepsFingerprintProfiles(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "fp.json", "", "fp@example.com")
	auth.Provider = "codex"
	identity := ResolveAuthSubjectIdentity(auth)
	legacyID := LegacyTenantScopedAuthSubjectID(auth)

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyID, "codex", "email", now.Add(-time.Hour))

	seedFingerprint := func(accountKey, profileKey string, seenAt time.Time) {
		t.Helper()
		if _, err := getDB().Exec(`
			INSERT INTO identity_fingerprints (
				tenant_id, provider, account_key, profile_key, auth_subject_id,
				client_product, client_variant, version, fields_json, observed_headers_json,
				created_at, updated_at, last_seen_at
			) VALUES (?, 'codex', ?, ?, ?, 'cli', '', '1', '{}', '{}', ?, ?, ?)
		`, sharedSubjectTenantA, accountKey, profileKey, accountKey,
			seenAt.Format(time.RFC3339Nano), seenAt.Format(time.RFC3339Nano), seenAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	// One profile exists on both sides (the shared row is newer and must win); a
	// second exists only on the legacy side and must survive the merge.
	seedFingerprint(legacyID, "default", now.Add(-2*time.Hour))
	seedFingerprint(identity.ID, "default", now.Add(-time.Minute))
	seedFingerprint(legacyID, "secondary", now.Add(-3*time.Hour))

	if err := MergeLegacySplitAuthSubject(auth, identity); err != nil {
		t.Fatal(err)
	}

	rows, err := getDB().Query(
		`SELECT profile_key, last_seen_at FROM identity_fingerprints WHERE account_key = ? ORDER BY profile_key`,
		identity.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	profiles := map[string]time.Time{}
	for rows.Next() {
		var profile string
		var seen storedTime
		if err := rows.Scan(&profile, &seen); err != nil {
			t.Fatal(err)
		}
		profiles[profile] = seen.Time.UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles after merge = %v, want default + secondary", profiles)
	}
	if !profiles["default"].Equal(now.Add(-time.Minute)) {
		t.Fatalf("default profile = %v, want the newer shared row", profiles["default"])
	}

	var leftovers int
	if err := getDB().QueryRow(
		`SELECT COUNT(*) FROM identity_fingerprints WHERE account_key = ?`, legacyID,
	).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("legacy fingerprints left behind: %d", leftovers)
	}
}

// Two genuinely different accounts must never be folded together.
func TestMergeLegacySplitSubjectKeepsDistinctAccountsApart(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "one.json", "", "one@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantA, "two.json", "", "two@example.com")
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if identityA.ID == identityB.ID {
		t.Fatal("different emails collapsed onto one subject")
	}

	now := time.Now().UTC().Truncate(time.Second)
	for _, seed := range []struct {
		auth     *coreauth.Auth
		identity *AuthSubjectIdentity
		requests int
	}{{authA, identityA, 2}, {authB, identityB, 6}} {
		legacyID := LegacyTenantScopedAuthSubjectID(seed.auth)
		seedLegacyAuthSubjectRow(t, legacyID, "codex", "email", now.Add(-time.Hour))
		for i := 0; i < seed.requests; i++ {
			projectSubjectRequest(t, legacyID, now.Add(-time.Hour), 10)
		}
		if err := MergeLegacySplitAuthSubject(seed.auth, seed.identity); err != nil {
			t.Fatal(err)
		}
	}

	if got := readSubjectSummary(t, identityA.ID); got.RequestTotal != 2 {
		t.Fatalf("account one = %+v, want 2 requests", got)
	}
	if got := readSubjectSummary(t, identityB.ID); got.RequestTotal != 6 {
		t.Fatalf("account two = %+v, want 6 requests", got)
	}
}

func TestLegacyAccountIDAuthSubjectIDOnlyForCodexUserSeed(t *testing.T) {
	oauth := codexMemberTestAuth(sharedSubjectTenantA, "oauth", "acct-1", "user-1", "a@example.com")
	identity := ResolveAuthSubjectIdentity(oauth)
	legacyID := LegacyAccountIDAuthSubjectID(oauth)
	if identity.SeedKind != "account_user_id" || legacyID == "" || legacyID == identity.ID {
		t.Fatalf("oauth predecessor = %q current = %q kind = %s", legacyID, identity.ID, identity.SeedKind)
	}

	accountOnly := sharedSubjectTestAuth(sharedSubjectTenantA, "acct-only", "acct-1", "a@example.com")
	if got := LegacyAccountIDAuthSubjectID(accountOnly); got != "" {
		t.Fatalf("account_id-only predecessor = %q, want empty", got)
	}

	apiKey := &coreauth.Auth{
		ID: sharedSubjectTenantA + "/codex:apikey:abc123", TenantID: sharedSubjectTenantA,
		Provider: "codex", FileName: "key.json",
	}
	if got := LegacyAccountIDAuthSubjectID(apiKey); got != "" {
		t.Fatalf("API key predecessor = %q, want empty", got)
	}
}

func leftoverAuthSubjectRows(t *testing.T, subjectID string) int {
	t.Helper()
	var n int
	if err := getDB().QueryRow(`
		SELECT (SELECT COUNT(*) FROM ai_account_subjects WHERE auth_subject_id = ?)
		     + (SELECT COUNT(*) FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ?)
		     + (SELECT COUNT(*) FROM ai_account_subject_status WHERE auth_subject_id = ?)
		     + (SELECT COUNT(*) FROM request_logs WHERE auth_subject_id = ?)
	`, subjectID, subjectID, subjectID, subjectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedActiveBindingOnSubject(t *testing.T, auth *coreauth.Auth, subjectID string, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339Nano)
	auth.EnsureIndex()
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_tenant_bindings (
			tenant_id, auth_id, auth_index, provider, auth_subject_id,
			binding_seed_kind, binding_seed_hash, share_eligible,
			binding_state, binding_revision, bound_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, 'account_id', 'legacy-hash', ?, 'active', 1, ?, ?)
	`, auth.TenantID, auth.ID, auth.Index, auth.Provider, subjectID, true, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// Production after the Team-collision seed change: a personal Pro account kept
// its week of traffic on the old account_id subject while the card read the
// new account_user_id row. Bindings had already moved, so the old subject had
// zero active bindings — the same shape a Docker host sees on the next boot
// after upgrading past that change.
func TestMergeLegacyAccountIDSubjectFoldsPersonalCodexUsage(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	auth := codexMemberTestAuth(sharedSubjectTenantA, "jd7.json", "acct-jd7", "user-jd7", "jd7@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	legacyID := LegacyAccountIDAuthSubjectID(auth)
	if legacyID == "" || legacyID == identity.ID {
		t.Fatalf("expected a distinct account_id predecessor, got %q / %q", legacyID, identity.ID)
	}

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyID, "codex", "account_id", now.Add(-48*time.Hour))
	for i := 0; i < 40; i++ {
		projectSubjectRequest(t, legacyID, now.Add(-24*time.Hour), 100)
	}
	if err := UpsertAIAccountSubject(identity); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		projectSubjectRequest(t, identity.ID, now.Add(-time.Hour), 20)
	}

	if err := MergeLegacySplitAuthSubject(auth, identity); err != nil {
		t.Fatal(err)
	}
	got := readSubjectSummary(t, identity.ID)
	if got.RequestTotal != 45 || got.SuccessTotal != 45 {
		t.Fatalf("merged personal Codex lifetime = %+v, want 45 requests", got)
	}
	if leftoverAuthSubjectRows(t, legacyID) != 0 {
		t.Fatalf("legacy account_id subject left rows behind")
	}

	resetMergedLegacySubjects()
	if err := MergeLegacySplitAuthSubject(auth, identity); err != nil {
		t.Fatal(err)
	}
	if again := readSubjectSummary(t, identity.ID); again.RequestTotal != 45 {
		t.Fatalf("re-running the account_id merge changed totals: %+v", again)
	}
}

func TestBindingHookMergesLegacyAccountIDSubjectOnAuthLoad(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	auth := codexMemberTestAuth(sharedSubjectTenantA, "hook-codex.json", "acct-hook", "user-hook", "hook@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	legacyID := LegacyAccountIDAuthSubjectID(auth)

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyID, "codex", "account_id", now.Add(-time.Hour))
	seedActiveBindingOnSubject(t, auth, legacyID, now.Add(-time.Hour))
	for i := 0; i < 8; i++ {
		projectSubjectRequest(t, legacyID, now.Add(-time.Hour), 25)
	}

	AIAccountBindingHook{}.OnAuthLoaded(context.Background(), auth)

	if got := readSubjectSummary(t, identity.ID); got.RequestTotal != 8 || got.SuccessTotal != 8 {
		t.Fatalf("hook did not migrate the Codex account_id subject: %+v", got)
	}
	bindings, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{auth.ID})
	if err != nil || len(bindings) != 1 || bindings[0].AuthSubjectID != identity.ID {
		t.Fatalf("binding=%+v err=%v, want it pointing at the account_user_id subject", bindings, err)
	}
}

// Two Team members still bound to the shared account_id subject must not have
// that pool folded onto whoever loads first — the rewrite would steal the
// other member's binding. Native and Docker upgrades of a Team workspace
// both look like this on first boot of the user-id seed.
func TestMergeLegacyAccountIDSubjectSkipsCrowdedTeamBindings(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()

	authA := codexMemberTestAuth(sharedSubjectTenantA, "team-a.json", "acct-team", "user-a", "a@example.com")
	authB := codexMemberTestAuth(sharedSubjectTenantA, "team-b.json", "acct-team", "user-b", "b@example.com")
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	legacyID := LegacyAccountIDAuthSubjectID(authA)
	if legacyID == "" || legacyID != LegacyAccountIDAuthSubjectID(authB) {
		t.Fatalf("team members must share the account_id predecessor: %q / %q", legacyID, LegacyAccountIDAuthSubjectID(authB))
	}
	if identityA.ID == identityB.ID {
		t.Fatal("team members collapsed onto one account_user_id subject")
	}

	now := time.Now().UTC().Truncate(time.Second)
	seedLegacyAuthSubjectRow(t, legacyID, "codex", "account_id", now.Add(-time.Hour))
	seedActiveBindingOnSubject(t, authA, legacyID, now.Add(-time.Hour))
	seedActiveBindingOnSubject(t, authB, legacyID, now.Add(-time.Hour))
	for i := 0; i < 12; i++ {
		projectSubjectRequest(t, legacyID, now.Add(-time.Hour), 10)
	}

	if err := MergeLegacySplitAuthSubject(authA, identityA); err != nil {
		t.Fatal(err)
	}
	if err := MergeLegacySplitAuthSubject(authB, identityB); err != nil {
		t.Fatal(err)
	}

	if got := readSubjectSummary(t, identityA.ID); got.RequestTotal != 0 {
		t.Fatalf("member A swallowed the shared pool: %+v", got)
	}
	if got := readSubjectSummary(t, identityB.ID); got.RequestTotal != 0 {
		t.Fatalf("member B swallowed the shared pool: %+v", got)
	}
	if got := readSubjectSummary(t, legacyID); got.RequestTotal != 12 {
		t.Fatalf("crowded account_id subject = %+v, want the 12 shared requests left in place", got)
	}
}
