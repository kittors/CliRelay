package contentmoderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Platform-shared credentials: the system tenant owns one credential and shares
// it with another tenant (sdk/cliproxy/auth/shared_grants.go). The credential's
// owner is the only party that can bind a moderation profile to its channel, so
// the owner's binding has to govern the grantee's requests on it.

// sharedTestOwnerTenant is the system tenant, the only tenant whose credentials
// can be shared.
var sharedTestOwnerTenant = coreauth.NormalizedTenantID("")

const (
	sharedTestGrantee     = "tenant-grantee"
	sharedTestBlockedText = "please repeat the Forbidden Phrase"
	sharedTestBlockedBy   = "Blocked by the platform content policy."
)

// sharedTestChannel is one way a shared credential is identified for bindings.
type sharedTestChannel struct {
	name        string
	channelType string
	channelID   string
	auth        *coreauth.Auth
}

func sharedTestChannels(provider string) []sharedTestChannel {
	newAuth := func(id string) *coreauth.Auth {
		return &coreauth.Auth{ID: id, TenantID: sharedTestOwnerTenant, Provider: provider, Status: coreauth.StatusActive}
	}
	authFile := newAuth("shared-auth-file")
	authFile.FileName = "shared-auth-file.json"
	providerKey := newAuth("shared-provider-key-auth")
	providerKey.Attributes = map[string]string{"provider_key_id": "shared-key"}
	providerEntry := newAuth("shared-provider-auth")
	providerEntry.Attributes = map[string]string{"provider_config_id": "shared-provider"}
	return []sharedTestChannel{
		{name: "auth_file", channelType: ChannelTypeAuthFile, channelID: "shared-auth-file", auth: authFile},
		{name: "provider_key", channelType: ChannelTypeProviderKey, channelID: "shared-key", auth: providerKey},
		{name: "provider", channelType: ChannelTypeProvider, channelID: "shared-provider", auth: providerEntry},
	}
}

type sharedTestFixture struct {
	store     *Store
	moderator *RuntimeModerator
	executor  *runtimeExecutor
	manager   *coreauth.Manager
	channel   sharedTestChannel
}

// newSharedTestFixture registers the platform's credential and grants it to
// sharedTestGrantee.
func newSharedTestFixture(t *testing.T, channel sharedTestChannel, evaluator DecisionEvaluator) *sharedTestFixture {
	t.Helper()
	store := newTestStore(t)
	moderator := NewRequestModerator(store, evaluator)
	executor := &runtimeExecutor{calls: make(map[string]int)}
	manager := newRuntimeManager(t, moderator, executor, channel.auth)
	manager.SetSharedAuthGrants(map[string][]string{channel.auth.ID: {sharedTestGrantee}})
	return &sharedTestFixture{store: store, moderator: moderator, executor: executor, manager: manager, channel: channel}
}

// bindOwnerProfile creates a profile in the owner's scope and binds it to the
// shared channel, the way the platform administrator does.
func (f *sharedTestFixture) bindOwnerProfile(t *testing.T, id string, input CreateProfileInput) Profile {
	t.Helper()
	profile := createRuntimeProfile(t, f.store, sharedTestOwnerTenant, id, input)
	bindRuntimeChannel(t, f.store, sharedTestOwnerTenant, f.channel.channelType, f.channel.channelID, profile.ID)
	return profile
}

func (f *sharedTestFixture) execute(ctx context.Context, tenantID, text string) error {
	return f.executeOptions(ctx, runtimeOptions(tenantID, text))
}

// executePinned sends the request through one specific credential. The manager
// spreads load across a tenant's candidates, so a test that cares which
// credential serves must pin it.
func (f *sharedTestFixture) executePinned(ctx context.Context, tenantID, authID, text string) error {
	opts := runtimeOptions(tenantID, text)
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	return f.executeOptions(ctx, opts)
}

func (f *sharedTestFixture) executeOptions(ctx context.Context, opts cliproxyexecutor.Options) error {
	_, err := f.manager.Execute(ctx, []string{f.executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, opts)
	return err
}

func (f *sharedTestFixture) executorCalls() int {
	f.executor.mu.Lock()
	defer f.executor.mu.Unlock()
	total := 0
	for _, calls := range f.executor.calls {
		total += calls
	}
	return total
}

func ownerKeywordProfile() CreateProfileInput {
	return CreateProfileInput{
		Name:            "Platform strict policy",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"forbidden phrase", "another confidential term"},
		BlockHTTPStatus: http.StatusUnavailableForLegalReasons,
		BlockMessage:    sharedTestBlockedBy,
	}
}

func requireContentPolicyViolation(t *testing.T, err error, wantStatus int, wantMessage string) {
	t.Helper()
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr.Code != "content_policy_violation" {
		t.Fatalf("error = %#v, want content policy violation", err)
	}
	if authErr.HTTPStatus != wantStatus || authErr.Message != wantMessage {
		t.Fatalf("violation = status %d %q, want %d %q", authErr.HTTPStatus, authErr.Message, wantStatus, wantMessage)
	}
}

func snapshotContext(t *testing.T) (context.Context, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	return context.WithValue(context.Background(), util.ContextKeyGin, ginCtx), ginCtx
}

// The platform bound a profile to its shared channel: a grantee's matching
// request is stopped before it reaches the credential. Each binding level has to
// work, because shared credentials are mostly OAuth auth files.
func TestSharedChannelEnforcesOwnersProfileOnGranteeRequests(t *testing.T) {
	for _, channel := range sharedTestChannels("moderation-runtime") {
		t.Run(channel.name, func(t *testing.T) {
			f := newSharedTestFixture(t, channel, NewEvaluator(nil))
			f.bindOwnerProfile(t, "owner-strict", ownerKeywordProfile())

			err := f.execute(context.Background(), sharedTestGrantee, sharedTestBlockedText)
			requireContentPolicyViolation(t, err, http.StatusUnavailableForLegalReasons, sharedTestBlockedBy)
			if calls := f.executorCalls(); calls != 0 {
				t.Fatalf("executor calls after a block = %d, want zero", calls)
			}

			if err = f.execute(context.Background(), sharedTestGrantee, "an ordinary question"); err != nil {
				t.Fatalf("request without a hit: %v", err)
			}
			if calls := f.executorCalls(); calls != 1 {
				t.Fatalf("executor calls = %d, want the clean request to reach the credential", calls)
			}

			if got := f.moderator.MetricsForTenant(sharedTestGrantee); got.Requests != 2 || got.Blocks != 1 || got.Allows != 1 || got.Errors != 0 {
				t.Fatalf("grantee metrics = %#v, want 2 requests, 1 block, 1 allow", got)
			}
			if got := f.moderator.MetricsForTenant(sharedTestOwnerTenant); got.Requests != 0 {
				t.Fatalf("owner metrics = %#v, want the grantee's traffic counted only for the grantee", got)
			}
		})
	}
}

// Nothing bound by the owner means nothing enforced: moderation stays opt-in,
// and a profile the grantee bound to some other channel of its own does not
// reach the platform's credential.
func TestSharedChannelWithoutOwnersBindingIsNotModerated(t *testing.T) {
	channel := sharedTestChannels("moderation-runtime")[0]
	f := newSharedTestFixture(t, channel, NewEvaluator(nil))
	granteeProfile := createRuntimeProfile(t, f.store, sharedTestGrantee, "grantee-strict", ownerKeywordProfile())
	bindRuntimeChannel(t, f.store, sharedTestGrantee, ChannelTypeProviderKey, "grantee-own-key", granteeProfile.ID)

	if err := f.execute(context.Background(), sharedTestGrantee, sharedTestBlockedText); err != nil {
		t.Fatalf("unbound shared channel blocked the request: %v", err)
	}
	if calls := f.executorCalls(); calls != 1 {
		t.Fatalf("executor calls = %d, want one", calls)
	}
	if got := f.moderator.MetricsForTenant(sharedTestGrantee); got.Requests != 1 || got.Allows != 1 || got.Blocks != 0 || got.Errors != 0 {
		t.Fatalf("grantee metrics = %#v, want one plain allow", got)
	}
}

// An owner profile that is switched off is the owner saying "do not moderate":
// the evaluator must not even be consulted.
func TestSharedChannelWithOwnersProfileOffIsNotModerated(t *testing.T) {
	channel := sharedTestChannels("moderation-runtime")[0]
	evaluator := &runtimeEvaluatorStub{decision: Decision{WouldBlock: true, Action: ActionKeywordBlock}}
	f := newSharedTestFixture(t, channel, evaluator)
	input := ownerKeywordProfile()
	input.Mode = ModeOff
	f.bindOwnerProfile(t, "owner-off", input)

	ctx, ginCtx := snapshotContext(t)
	if err := f.execute(ctx, sharedTestGrantee, sharedTestBlockedText); err != nil {
		t.Fatalf("request on a channel whose profile is off: %v", err)
	}
	if evaluator.calls.Load() != 0 {
		t.Fatalf("evaluator calls = %d, want zero for a profile that is off", evaluator.calls.Load())
	}
	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok || snapshot.Evaluated || snapshot.WouldBlock || snapshot.Action != ActionAllow {
		t.Fatalf("snapshot = %#v (present=%v), want an unevaluated allow", snapshot, ok)
	}
}

// A profile cannot be deleted while it is bound, so "deleted" reaches the
// runtime as: unbind, delete. Until then the owner's policy keeps applying; after
// it the shared channel is open again rather than failing.
func TestSharedChannelFollowsOwnerUnbindingAndDeletingTheProfile(t *testing.T) {
	channel := sharedTestChannels("moderation-runtime")[1]
	f := newSharedTestFixture(t, channel, NewEvaluator(nil))
	profile := f.bindOwnerProfile(t, "owner-strict", ownerKeywordProfile())

	requireContentPolicyViolation(t, f.execute(context.Background(), sharedTestGrantee, sharedTestBlockedText), http.StatusUnavailableForLegalReasons, sharedTestBlockedBy)

	if err := f.store.DeleteProfile(context.Background(), sharedTestOwnerTenant, profile.ID); !errors.Is(err, ErrProfileBound) {
		t.Fatalf("DeleteProfile of a bound profile = %v, want ErrProfileBound", err)
	}
	requireContentPolicyViolation(t, f.execute(context.Background(), sharedTestGrantee, sharedTestBlockedText), http.StatusUnavailableForLegalReasons, sharedTestBlockedBy)

	if err := f.store.DeleteChannelBinding(context.Background(), sharedTestOwnerTenant, channel.channelType, channel.channelID); err != nil {
		t.Fatalf("DeleteChannelBinding: %v", err)
	}
	if err := f.store.DeleteProfile(context.Background(), sharedTestOwnerTenant, profile.ID); err != nil {
		t.Fatalf("DeleteProfile after unbind: %v", err)
	}
	if err := f.execute(context.Background(), sharedTestGrantee, sharedTestBlockedText); err != nil {
		t.Fatalf("request after the owner removed the policy: %v", err)
	}
	if got := f.moderator.MetricsForTenant(sharedTestGrantee); got.Blocks != 2 || got.Allows != 1 || got.Errors != 0 {
		t.Fatalf("grantee metrics = %#v, want 2 blocks then 1 allow and no errors", got)
	}
}

// Credentials a tenant owns keep resolving in that tenant's own scope. The
// platform's binding is for the platform's channel only, even when a channel id
// of the tenant's happens to be the same string.
func TestOwnCredentialsKeepUsingTheirOwnersProfile(t *testing.T) {
	const ownMessage = "Blocked by the tenant's own policy."
	channel := sharedTestChannels("moderation-runtime")[1]
	f := newSharedTestFixture(t, channel, NewEvaluator(nil))
	f.bindOwnerProfile(t, "owner-strict", ownerKeywordProfile())

	ownAuth := &coreauth.Auth{
		ID:         "grantee-own-auth",
		TenantID:   sharedTestGrantee,
		Provider:   f.executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"provider_key_id": channel.channelID},
	}
	f.manager.RegisterExecutorForTenant(sharedTestGrantee, f.executor)
	if _, err := f.manager.Register(context.Background(), ownAuth); err != nil {
		t.Fatalf("Register own credential: %v", err)
	}

	// No profile of the tenant's own is bound: the platform's binding on the same
	// channel id must not leak onto the tenant's credential.
	if err := f.executePinned(context.Background(), sharedTestGrantee, ownAuth.ID, sharedTestBlockedText); err != nil {
		t.Fatalf("own credential was blocked by the platform's profile: %v", err)
	}
	f.executor.mu.Lock()
	ownCalls := f.executor.calls[ownAuth.ID]
	f.executor.mu.Unlock()
	if ownCalls != 1 {
		t.Fatalf("own credential calls = %d, want the request served by it", ownCalls)
	}

	ownProfile := ownerKeywordProfile()
	ownProfile.Name = "Tenant policy"
	ownProfile.BlockedKeywords = []string{"forbidden phrase"}
	ownProfile.BlockHTTPStatus = http.StatusForbidden
	ownProfile.BlockMessage = ownMessage
	created := createRuntimeProfile(t, f.store, sharedTestGrantee, "grantee-strict", ownProfile)
	bindRuntimeChannel(t, f.store, sharedTestGrantee, ChannelTypeProviderKey, channel.channelID, created.ID)
	requireContentPolicyViolation(t, f.executePinned(context.Background(), sharedTestGrantee, ownAuth.ID, sharedTestBlockedText), http.StatusForbidden, ownMessage)
	// The shared credential still answers to the platform's policy, not the tenant's.
	requireContentPolicyViolation(t, f.executePinned(context.Background(), sharedTestGrantee, channel.auth.ID, sharedTestBlockedText), http.StatusUnavailableForLegalReasons, sharedTestBlockedBy)
}

// The moderator must look the binding up in the credential owner's scope, with
// the credential's own channel ids, whoever sent the request.
func TestRuntimeModeratorResolvesBindingInCredentialOwnersScope(t *testing.T) {
	var gotTenant, gotAuthFile, gotKey, gotProvider string
	resolver := &recordingResolver{fn: func(tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error) {
		gotTenant, gotAuthFile, gotKey, gotProvider = tenantID, authFileID, providerKeyID, providerID
		return Profile{}, "", ErrNotFound
	}}
	moderator := NewRequestModerator(resolver, &runtimeEvaluatorStub{})
	auth := &coreauth.Auth{
		ID:         "shared-auth",
		TenantID:   "tenant-owner",
		FileName:   "shared-auth.json",
		Attributes: map[string]string{"provider_key_id": "key-1", "provider_config_id": "provider-1"},
	}

	if result := moderator.Moderate(context.Background(), auth, runtimeOptions("tenant-requester", "hello")); result.Blocked {
		t.Fatal("no binding must not block")
	}
	if gotTenant != "tenant-owner" || gotAuthFile != "shared-auth" || gotKey != "key-1" || gotProvider != "provider-1" {
		t.Fatalf("resolved in tenant=%q authFile=%q key=%q provider=%q, want the owner's scope and the credential's channel ids", gotTenant, gotAuthFile, gotKey, gotProvider)
	}
	if moderator.Metrics().Errors != 0 {
		t.Fatalf("metrics = %#v, a credential shared across tenants is not an error", moderator.Metrics())
	}
	if got := moderator.MetricsForTenant("tenant-requester"); got.Requests != 1 || got.Allows != 1 {
		t.Fatalf("requester metrics = %#v, want the request counted for the requester", got)
	}
	if got := moderator.MetricsForTenant("tenant-owner"); got.Requests != 0 {
		t.Fatalf("owner metrics = %#v, want nothing counted for the owner", got)
	}
}

type recordingResolver struct {
	fn func(tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error)
}

func (r *recordingResolver) ResolveProfile(_ context.Context, tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error) {
	return r.fn(tenantID, authFileID, providerKeyID, providerID)
}

// The block is recorded for the tenant that sent the request, and what lands in
// that tenant's request log is the verdict, not the owner's configuration.
func TestSharedChannelVerdictIsRecordedForRequesterWithoutOwnersConfiguration(t *testing.T) {
	channel := sharedTestChannels("moderation-runtime")[0]
	f := newSharedTestFixture(t, channel, NewEvaluator(nil))
	profile := f.bindOwnerProfile(t, "owner-strict", ownerKeywordProfile())
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	ctx, ginCtx := snapshotContext(t)
	requireContentPolicyViolation(t, f.execute(ctx, sharedTestGrantee, sharedTestBlockedText), http.StatusUnavailableForLegalReasons, sharedTestBlockedBy)

	var blocked *log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Message == "content moderation blocked request" {
			blocked = entry
		}
	}
	if blocked == nil {
		t.Fatalf("no block was logged; entries: %v", hook.AllEntries())
	}
	if blocked.Data["tenant_id"] != sharedTestGrantee {
		t.Fatalf("logged tenant_id = %v, want the requester %q", blocked.Data["tenant_id"], sharedTestGrantee)
	}
	if blocked.Data["policy_tenant_id"] != sharedTestOwnerTenant {
		t.Fatalf("logged policy_tenant_id = %v, want the owner %q", blocked.Data["policy_tenant_id"], sharedTestOwnerTenant)
	}
	if blocked.Data["profile_id"] != profile.ID || blocked.Data["channel_id"] != channel.channelID {
		t.Fatalf("operator log lost the profile or channel: %v", blocked.Data)
	}

	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok {
		t.Fatal("no moderation snapshot recorded for the request log")
	}
	if !snapshot.Evaluated || !snapshot.WouldBlock || snapshot.Action != ActionKeywordBlock || snapshot.MatchedKeyword != "forbidden phrase" {
		t.Fatalf("snapshot lost the verdict: %#v", snapshot)
	}
	if snapshot.ResolutionSource != ResolutionSourceCredentialOwner {
		t.Fatalf("resolution source = %q, want %q", snapshot.ResolutionSource, ResolutionSourceCredentialOwner)
	}
	if snapshot.ProfileID != "" || snapshot.ProfileName != "" || snapshot.ProfileVersion != 0 || snapshot.Backend != "" || snapshot.ChannelType != "" || snapshot.ChannelID != "" {
		t.Fatalf("snapshot identifies the owner's configuration: %#v", snapshot)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, secret := range []string{profile.ID, profile.Name, "another confidential term", channel.channelID} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot %s exposes %q", raw, secret)
		}
	}
}

// Same-tenant requests keep the full snapshot: the profile is the tenant's own.
func TestOwnChannelSnapshotKeepsProfileIdentity(t *testing.T) {
	const tenantID = "tenant-own-snapshot"
	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "own-strict", ownerKeywordProfile())
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "own-key", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(nil))
	auth := &coreauth.Auth{ID: "own-auth", TenantID: tenantID, Attributes: map[string]string{"provider_key_id": "own-key"}}

	ctx, ginCtx := snapshotContext(t)
	if result := moderator.Moderate(ctx, auth, runtimeOptions(tenantID, sharedTestBlockedText)); !result.Blocked {
		t.Fatal("own channel was not moderated")
	}
	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok {
		t.Fatal("no snapshot")
	}
	if snapshot.ProfileID != profile.ID || snapshot.ProfileName != profile.Name || snapshot.ProfileVersion != profile.Version ||
		snapshot.ResolutionSource != ChannelTypeProviderKey || snapshot.ChannelType != ChannelTypeProviderKey || snapshot.ChannelID != "own-key" {
		t.Fatalf("snapshot = %#v, want the tenant's own profile and channel", snapshot)
	}
	// An own channel never carries the owner-policy tag in the operator log either.
	if _, tagged := moderationLogFields(tenantID, auth, ChannelTypeProviderKey, profile)["policy_tenant_id"]; tagged {
		t.Fatal("operator log marks an own channel as another tenant's policy")
	}
}

// The owner's profile may use a moderation API. The grantee's text is judged by
// the owner's endpoint and key, and neither the address nor the key (nor the
// owner's other settings) comes back to the grantee.
func TestSharedChannelUsesOwnersModerationAPIWithoutExposingIt(t *testing.T) {
	const ownerAPIKey = "owner-moderation-key"
	var calls atomic.Int32
	var authorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		authorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"violence":0.99}}]}`))
	}))
	defer server.Close()

	channel := sharedTestChannels("moderation-runtime")[0]
	f := newSharedTestFixture(t, channel, NewEvaluator(server.Client()))
	f.bindOwnerProfile(t, "owner-api", CreateProfileInput{
		Name:            "Platform API policy",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeAPIOnly,
		APIKey:          ownerAPIKey,
		BaseURL:         server.URL,
		BlockHTTPStatus: http.StatusForbidden,
		BlockMessage:    sharedTestBlockedBy,
	})

	ctx, ginCtx := snapshotContext(t)
	err := f.execute(ctx, sharedTestGrantee, "some text the API flags")
	requireContentPolicyViolation(t, err, http.StatusForbidden, sharedTestBlockedBy)
	if calls.Load() != 1 || authorization.Load() != "Bearer "+ownerAPIKey {
		t.Fatalf("moderation API calls = %d with authorization %v, want one call with the owner's key", calls.Load(), authorization.Load())
	}
	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok || !snapshot.WouldBlock || snapshot.Action != ActionAPIBlock || snapshot.HighestCategory != "violence" {
		t.Fatalf("snapshot = %#v (present=%v), want the API verdict and category", snapshot, ok)
	}
	raw, _ := json.Marshal(snapshot)
	for _, secret := range []string{ownerAPIKey, server.URL, "Platform API policy", string(BackendOpenAIModerations)} {
		if strings.Contains(string(raw)+err.Error(), secret) {
			t.Fatalf("grantee-visible record exposes %q: %s / %v", secret, raw, err)
		}
	}
}

// A broken moderation dependency still fails open, now also for shared
// channels, and the failure is counted for the requester whose request it was.
func TestSharedChannelModerationAPIFailureFailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	channel := sharedTestChannels("moderation-runtime")[0]
	f := newSharedTestFixture(t, channel, NewEvaluator(server.Client()))
	f.bindOwnerProfile(t, "owner-api", CreateProfileInput{
		Name:        "Platform API policy",
		Mode:        ModePreBlock,
		KeywordMode: KeywordModeAPIOnly,
		APIKey:      "owner-moderation-key",
		BaseURL:     server.URL,
	})

	ctx, ginCtx := snapshotContext(t)
	if err := f.execute(ctx, sharedTestGrantee, "hello"); err != nil {
		t.Fatalf("moderation outage must not take shared traffic down: %v", err)
	}
	if calls := f.executorCalls(); calls != 1 {
		t.Fatalf("executor calls = %d, want one", calls)
	}
	if got := f.moderator.MetricsForTenant(sharedTestGrantee); got.Errors != 1 || got.Allows != 1 {
		t.Fatalf("grantee metrics = %#v, want one fail-open error and one allow", got)
	}
	if got := f.moderator.MetricsForTenant(sharedTestOwnerTenant); got.Requests != 0 || got.Errors != 0 {
		t.Fatalf("owner metrics = %#v, want nothing", got)
	}
	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok || snapshot.ErrorClass != "upstream_status" {
		t.Fatalf("snapshot = %#v (present=%v), want the error class", snapshot, ok)
	}
	if raw, _ := json.Marshal(snapshot); strings.Contains(string(raw), server.URL) {
		t.Fatalf("snapshot exposes the owner's moderation endpoint: %s", raw)
	}
}

// When the profile store is unreachable the last resolution of the same owner
// binding keeps applying, exactly as it does for a tenant's own channels; the
// failure is counted for the tenant whose request it was.
func TestSharedChannelKeepsOwnersLastGoodProfileOnStoreError(t *testing.T) {
	profile, err := NewProfile(sharedTestOwnerTenant, "owner-strict", ownerKeywordProfile(), time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	var call atomic.Int32
	resolver := &recordingResolver{fn: func(string, string, string, string) (Profile, string, error) {
		if call.Add(1) == 1 {
			return profile, ChannelTypeProviderKey, nil
		}
		return Profile{}, "", errors.New("database unavailable")
	}}
	moderator := NewRequestModerator(resolver, NewEvaluator(nil))
	auth := sharedTestChannels("moderation-runtime")[1].auth
	for i := 0; i < 2; i++ {
		if result := moderator.Moderate(context.Background(), auth, runtimeOptions(sharedTestGrantee, sharedTestBlockedText)); !result.Blocked {
			t.Fatalf("call %d was not blocked", i+1)
		}
	}
	if got := moderator.MetricsForTenant(sharedTestGrantee); got.Errors != 1 || got.Blocks != 2 {
		t.Fatalf("grantee metrics = %#v, want 2 blocks and the store error", got)
	}
	if got := moderator.MetricsForTenant(sharedTestOwnerTenant); got.Requests != 0 {
		t.Fatalf("owner metrics = %#v, want nothing", got)
	}
}
