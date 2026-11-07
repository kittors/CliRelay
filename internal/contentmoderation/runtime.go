package contentmoderation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const requestDecisionCacheMetadataKey = "content_moderation_decision_cache"

type ProfileResolver interface {
	ResolveProfile(ctx context.Context, tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error)
}

type DecisionEvaluator interface {
	Evaluate(ctx context.Context, profile Profile, input Input) Decision
}

type RuntimeModerator struct {
	resolver  ProfileResolver
	evaluator DecisionEvaluator
	// words supplies the platform, tenant and API key word policies (see
	// wordguard_runtime.go). It is the profile resolver itself when that can also
	// load them; nil disables the word guard.
	words        WordPolicyResolver
	wordContexts wordContextCache
	// logs spaces the word guard's process log lines (see logLimiter).
	logs logLimiter
	// wordsOff is the operator's switch for the word guard (see WordGuardEnvVar).
	wordsOff atomic.Bool
	// wordLoadTimeout is how long a request waits for its policies; zero means
	// wordPolicyWait. Tests shorten or lengthen it.
	wordLoadTimeout time.Duration
	// regexWork is the work budget of one request's regular expressions, in steps;
	// zero means maxRegexWork. Tests shrink it, because spending the real one
	// takes half a second of CPU.
	regexWork int64
	// secretMasking holds the tenants' secret masking settings; nil means the
	// process's registry (SecretMasking). Tests supply their own.
	secretMasking *SecretMaskingRegistry

	lastGoodMu sync.RWMutex
	lastGood   map[resolutionKey]resolvedProfile

	// Per-tenant process-local counters. Management metrics must never cross tenants.
	tenantMetrics sync.Map // map[string]*tenantMetricCounters
}

type tenantMetricCounters struct {
	requests       atomic.Uint64
	allows         atomic.Uint64
	blocks         atomic.Uint64
	errors         atomic.Uint64
	cacheHits      atomic.Uint64
	inFlight       atomic.Int64
	latencyTotalMS atomic.Uint64
	latencySamples atomic.Uint64
}

// resolutionKey identifies one channel binding lookup. tenantID is the tenant
// that owns the binding (the credential's owner), not the one sending the request.
type resolutionKey struct {
	tenantID      string
	authFileID    string
	providerKeyID string
	providerID    string
}

type resolvedProfile struct {
	profile Profile
	source  string
}

type requestDecisionCache struct {
	mu        sync.Mutex
	decisions map[string]Decision
}

type ModerationMetrics struct {
	Requests       uint64  `json:"requests"`
	Allows         uint64  `json:"allows"`
	Blocks         uint64  `json:"blocks"`
	Errors         uint64  `json:"errors"`
	CacheHits      uint64  `json:"cache_hits"`
	InFlight       int64   `json:"in_flight"`
	LatencyTotalMS uint64  `json:"latency_total_ms"`
	LatencySamples uint64  `json:"latency_samples"`
	AvgLatencyMS   float64 `json:"avg_latency_ms"`
}

var runtimeModerator atomic.Pointer[RuntimeModerator]

// SetRuntime exposes the live proxy moderator to process-local management metrics.
func SetRuntime(moderator *RuntimeModerator) {
	runtimeModerator.Store(moderator)
}

// Runtime returns the moderator used by the live proxy process.
func Runtime() *RuntimeModerator {
	return runtimeModerator.Load()
}

func NewRequestModerator(resolver ProfileResolver, evaluator DecisionEvaluator) *RuntimeModerator {
	if evaluator == nil {
		evaluator = NewEvaluator(nil)
	}
	moderator := &RuntimeModerator{
		resolver:  resolver,
		evaluator: evaluator,
		lastGood:  make(map[resolutionKey]resolvedProfile),
	}
	if words, ok := resolver.(WordPolicyResolver); ok {
		moderator.words = words
	}
	if wordGuardSwitchedOff() {
		moderator.wordsOff.Store(true)
		log.Warnf("word guard is switched off by %s: requests are not checked against word policies", WordGuardEnvVar)
	}
	return moderator
}

func (m *RuntimeModerator) counters(tenantID string) *tenantMetricCounters {
	if m == nil {
		return &tenantMetricCounters{}
	}
	key := coreauth.NormalizedTenantID(tenantID)
	if key == "" {
		key = "unknown"
	}
	if existing, ok := m.tenantMetrics.Load(key); ok {
		return existing.(*tenantMetricCounters)
	}
	created := &tenantMetricCounters{}
	actual, _ := m.tenantMetrics.LoadOrStore(key, created)
	return actual.(*tenantMetricCounters)
}

func snapshotMetrics(c *tenantMetricCounters) ModerationMetrics {
	if c == nil {
		return ModerationMetrics{}
	}
	latencyTotalMS := c.latencyTotalMS.Load()
	latencySamples := c.latencySamples.Load()
	metrics := ModerationMetrics{
		Requests:       c.requests.Load(),
		Allows:         c.allows.Load(),
		Blocks:         c.blocks.Load(),
		Errors:         c.errors.Load(),
		CacheHits:      c.cacheHits.Load(),
		InFlight:       c.inFlight.Load(),
		LatencyTotalMS: latencyTotalMS,
		LatencySamples: latencySamples,
	}
	if latencySamples > 0 {
		metrics.AvgLatencyMS = float64(latencyTotalMS) / float64(latencySamples)
	}
	return metrics
}

// MetricsForTenant returns process-local counters for one tenant only.
func (m *RuntimeModerator) MetricsForTenant(tenantID string) ModerationMetrics {
	if m == nil {
		return ModerationMetrics{}
	}
	key := coreauth.NormalizedTenantID(tenantID)
	if key == "" {
		return ModerationMetrics{}
	}
	existing, ok := m.tenantMetrics.Load(key)
	if !ok {
		return ModerationMetrics{}
	}
	return snapshotMetrics(existing.(*tenantMetricCounters))
}

// Metrics is retained for tests that inspect a single-tenant moderator instance.
func (m *RuntimeModerator) Metrics() ModerationMetrics {
	if m == nil {
		return ModerationMetrics{}
	}
	var out ModerationMetrics
	m.tenantMetrics.Range(func(_, value any) bool {
		part := snapshotMetrics(value.(*tenantMetricCounters))
		out.Requests += part.Requests
		out.Allows += part.Allows
		out.Blocks += part.Blocks
		out.Errors += part.Errors
		out.CacheHits += part.CacheHits
		out.InFlight += part.InFlight
		out.LatencyTotalMS += part.LatencyTotalMS
		out.LatencySamples += part.LatencySamples
		return true
	})
	if out.LatencySamples > 0 {
		out.AvgLatencyMS = float64(out.LatencyTotalMS) / float64(out.LatencySamples)
	}
	return out
}

func (m *RuntimeModerator) Moderate(ctx context.Context, auth *coreauth.Auth, opts cliproxyexecutor.Options) coreauth.RequestModerationResult {
	if m == nil || auth == nil || m.resolver == nil || m.evaluator == nil {
		return coreauth.RequestModerationResult{}
	}
	// Two tenants can be involved. tenantID sent the request and owns the record of
	// it: metrics, logs and the request log. The credential's owner owns the policy:
	// a profile binds to a channel, and a channel belongs to whoever owns the
	// credential, so the binding is resolved in the owner's scope.
	//
	// They differ for a platform-shared credential (sdk/cliproxy/auth/shared_grants.go),
	// which stays one account on its owner's executor, so its owner's policy follows
	// it whoever asks. Resolving in the requester's scope found nothing, because a
	// tenant cannot bind a profile to a channel it does not own, and used to skip
	// moderation altogether.
	//
	// No tenant-wide or API-key profile exists yet, so for a shared credential the
	// owner's binding is the only policy there is. A requester-side layer
	// (platform -> tenant -> key scopes) would be evaluated in addition to it.
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	counters := m.counters(tenantID)
	counters.requests.Add(1)

	authFileID, providerKeyID, providerID := runtimeChannelIDs(auth)
	key := resolutionKey{tenantID: credentialTenantID(auth), authFileID: authFileID, providerKeyID: providerKeyID, providerID: providerID}
	resolved, usedLastGood, err := m.resolve(ctx, key)
	if errors.Is(err, ErrNotFound) {
		counters.allows.Add(1)
		return coreauth.RequestModerationResult{}
	}
	if err != nil {
		m.recordError(tenantID, auth, "", Profile{}, "store_error", 0, false)
		return coreauth.RequestModerationResult{}
	}
	if usedLastGood {
		m.recordError(tenantID, auth, resolved.source, resolved.profile, "store_error_last_good", 0, false)
	}
	profile := resolved.profile
	if profile.Mode == ModeOff {
		counters.allows.Add(1)
		decision := Decision{Action: ActionAllow}
		m.setSnapshot(ctx, tenantID, auth, resolved.source, profile, decision, false, false)
		m.logDecision(tenantID, auth, resolved.source, profile, decision, false)
		return coreauth.RequestModerationResult{}
	}

	input := ExtractModerationInput(opts.SourceFormat, opts.OriginalRequest)
	if input.Empty() {
		counters.allows.Add(1)
		decision := Decision{Action: ActionAllow}
		m.setSnapshot(ctx, tenantID, auth, resolved.source, profile, decision, false, false)
		m.logDecision(tenantID, auth, resolved.source, profile, decision, false)
		return coreauth.RequestModerationResult{}
	}

	cacheKey := decisionCacheKey(profile, input)
	cache := decisionCacheFromMetadata(opts.Metadata)
	if cache != nil {
		if decision, ok := cache.get(cacheKey); ok {
			counters.cacheHits.Add(1)
			return m.resultForDecision(ctx, tenantID, auth, resolved.source, profile, decision, true)
		}
	}
	apiPath := moderationAPIPath(profile, input)
	if apiPath {
		counters.inFlight.Add(1)
	}
	decision := m.evaluator.Evaluate(ctx, profile, input)
	if apiPath {
		counters.inFlight.Add(-1)
		if decision.LatencyMS >= 0 {
			counters.latencyTotalMS.Add(uint64(decision.LatencyMS))
			counters.latencySamples.Add(1)
		}
	}
	if cache != nil {
		cache.put(cacheKey, decision)
	}
	return m.resultForDecision(ctx, tenantID, auth, resolved.source, profile, decision, false)
}

func (m *RuntimeModerator) resolve(ctx context.Context, key resolutionKey) (resolvedProfile, bool, error) {
	profile, source, err := m.resolver.ResolveProfile(ctx, key.tenantID, key.authFileID, key.providerKeyID, key.providerID)
	if err == nil {
		resolved := resolvedProfile{profile: profile, source: source}
		m.lastGoodMu.Lock()
		m.lastGood[key] = resolved
		m.lastGoodMu.Unlock()
		return resolved, false, nil
	}
	if errors.Is(err, ErrNotFound) {
		m.lastGoodMu.Lock()
		delete(m.lastGood, key)
		m.lastGoodMu.Unlock()
		return resolvedProfile{}, false, ErrNotFound
	}
	m.lastGoodMu.RLock()
	resolved, ok := m.lastGood[key]
	m.lastGoodMu.RUnlock()
	if ok {
		return resolved, true, nil
	}
	return resolvedProfile{}, false, err
}

func (m *RuntimeModerator) resultForDecision(ctx context.Context, tenantID string, auth *coreauth.Auth, source string, profile Profile, decision Decision, cached bool) coreauth.RequestModerationResult {
	counters := m.counters(tenantID)
	if decision.Action == ActionAPIError {
		counters.allows.Add(1)
		errorClass := moderationErrorClass(decision.ModerationError)
		m.setSnapshot(ctx, tenantID, auth, source, profile, decision, true, cached)
		m.recordError(tenantID, auth, source, profile, errorClass, decision.LatencyMS, cached)
		return coreauth.RequestModerationResult{}
	}
	m.setSnapshot(ctx, tenantID, auth, source, profile, decision, true, cached)
	if decision.WouldBlock {
		counters.blocks.Add(1)
		m.logDecision(tenantID, auth, source, profile, decision, cached)
		return coreauth.RequestModerationResult{Blocked: true, Message: profile.BlockMessage, HTTPStatus: profile.BlockHTTPStatus}
	}
	counters.allows.Add(1)
	m.logDecision(tenantID, auth, source, profile, decision, cached)
	return coreauth.RequestModerationResult{}
}

func (m *RuntimeModerator) recordError(tenantID string, auth *coreauth.Auth, source string, profile Profile, errorClass string, latencyMS int64, cached bool) {
	m.counters(tenantID).errors.Add(1)
	fields := moderationLogFields(tenantID, auth, source, profile)
	fields["action"] = ActionAPIError
	fields["error_class"] = errorClass
	fields["latency_ms"] = latencyMS
	fields["cache_hit"] = cached
	log.WithFields(fields).Warn("content moderation failed open")
}

func (m *RuntimeModerator) logDecision(tenantID string, auth *coreauth.Auth, source string, profile Profile, decision Decision, cached bool) {
	fields := moderationLogFields(tenantID, auth, source, profile)
	fields["action"] = decision.Action
	fields["latency_ms"] = decision.LatencyMS
	fields["cache_hit"] = cached
	if decision.Action == ActionKeywordBlock {
		fields["category"] = "keyword"
		fields["score"] = 1.0
		fields["matched_source"] = decision.MatchedSource
	} else if decision.HighestCategory != "" {
		fields["category"] = decision.HighestCategory
		fields["score"] = decision.HighestScore
	}
	// A guard verdict can block without naming an enabled category (unknown or
	// unnamed risk), so the label is logged independently of category/score.
	if decision.Safety != "" {
		fields["safety"] = decision.Safety
	}
	if len(decision.MatchedScanners) > 0 {
		fields["matched_scanners"] = strings.Join(decision.MatchedScanners, ",")
	}
	entry := log.WithFields(fields)
	if decision.WouldBlock {
		entry.Warn("content moderation blocked request")
	} else {
		entry.Debug("content moderation allowed request")
	}
}

func moderationLogFields(tenantID string, auth *coreauth.Auth, source string, profile Profile) log.Fields {
	channelType, channelID := resolvedChannel(auth, source)
	fields := log.Fields{
		"tenant_id":         tenantID,
		"profile_id":        profile.ID,
		"profile_version":   profile.Version,
		"resolution_source": source,
		"channel_type":      channelType,
		"channel_id":        channelID,
	}
	if policyTenantID := credentialTenantID(auth); policyTenantID != tenantID {
		// Operator log only: whose profile ran on this tenant's request.
		fields["policy_tenant_id"] = policyTenantID
	}
	return fields
}

// credentialTenantID is the tenant that owns the credential, and with it the
// channel bindings that govern requests running on it. Credentials stored without
// a tenant belong to the system tenant, as everywhere else.
func credentialTenantID(auth *coreauth.Auth) string {
	if auth == nil {
		return coreauth.NormalizedTenantID("")
	}
	return coreauth.NormalizedTenantID(auth.TenantID)
}

func resolvedChannel(auth *coreauth.Auth, source string) (string, string) {
	authFileID, providerKeyID, providerID := runtimeChannelIDs(auth)
	switch source {
	case ChannelTypeAuthFile:
		return source, authFileID
	case ChannelTypeProviderKey:
		return source, providerKeyID
	case ChannelTypeProvider:
		return source, providerID
	default:
		return "", ""
	}
}

func runtimeChannelIDs(auth *coreauth.Auth) (authFileID, providerKeyID, providerID string) {
	if auth == nil {
		return "", "", ""
	}
	if strings.TrimSpace(auth.FileName) != "" || strings.TrimSpace(auth.Attributes["path"]) != "" {
		authFileID = strings.TrimSpace(auth.Attributes["gemini_virtual_parent"])
		if authFileID == "" {
			authFileID = strings.TrimSpace(auth.ID)
		}
	}
	providerKeyID = strings.TrimSpace(auth.Attributes["provider_key_id"])
	providerID = strings.TrimSpace(auth.Attributes["provider_config_id"])
	return authFileID, providerKeyID, providerID
}

// moderationAPIPath predicts whether Evaluate will call the backend, so the
// in-flight gauge and latency samples count backend calls only. The keyword
// verdict is memoized on the input, so the evaluator does not scan again.
func moderationAPIPath(profile Profile, input Input) bool {
	if profile.Mode != ModePreBlock || profile.KeywordMode == KeywordModeKeywordOnly || !input.hasBackendText(profile.APIIncludesToolOutput) {
		return false
	}
	if profile.KeywordMode == KeywordModeKeywordAndAPI {
		return !input.matchKeywords(profileKeywordSet(profile)).matched
	}
	return true
}

func decisionCacheKey(profile Profile, input Input) string {
	return fmt.Sprintf("%s:%d:%x", profile.ID, profile.Version, input.digest())
}

func decisionCacheFromMetadata(metadata map[string]any) *requestDecisionCache {
	if metadata == nil {
		return nil
	}
	if cache, ok := metadata[requestDecisionCacheMetadataKey].(*requestDecisionCache); ok && cache != nil {
		return cache
	}
	cache := &requestDecisionCache{decisions: make(map[string]Decision)}
	metadata[requestDecisionCacheMetadataKey] = cache
	return cache
}

func (c *requestDecisionCache) get(key string) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	decision, ok := c.decisions[key]
	return decision, ok
}

func (c *requestDecisionCache) put(key string, decision Decision) {
	c.mu.Lock()
	c.decisions[key] = decision
	c.mu.Unlock()
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func moderationErrorClass(message string) string {
	message = strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(message, "deadline exceeded") || strings.Contains(message, "timeout"):
		return "timeout"
	case strings.Contains(message, "returned status"):
		return "upstream_status"
	case strings.Contains(message, "decode") ||
		strings.Contains(message, "missing category scores") ||
		strings.Contains(message, "not a valid verdict") ||
		strings.Contains(message, "exceeds size limit"):
		return "invalid_response"
	case strings.Contains(message, "request failed"):
		return "transport"
	case strings.Contains(message, "base url"):
		return "invalid_config"
	default:
		return "moderation_error"
	}
}
