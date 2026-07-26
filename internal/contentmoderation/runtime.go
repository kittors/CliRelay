package contentmoderation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const requestDecisionCacheMetadataKey = "content_moderation_decision_cache"

type ProfileResolver interface {
	ResolveProfile(ctx context.Context, tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error)
}

type DecisionEvaluator interface {
	Evaluate(ctx context.Context, profile Profile, input string) Decision
}

type RuntimeModerator struct {
	resolver  ProfileResolver
	evaluator DecisionEvaluator

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
	return &RuntimeModerator{
		resolver:  resolver,
		evaluator: evaluator,
		lastGood:  make(map[resolutionKey]resolvedProfile),
	}
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
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	counters := m.counters(tenantID)
	counters.requests.Add(1)
	if coreauth.NormalizedTenantID(auth.TenantID) != tenantID {
		m.recordError(tenantID, auth, "", Profile{}, "tenant_mismatch", 0, false)
		return coreauth.RequestModerationResult{}
	}

	authFileID, providerKeyID, providerID := runtimeChannelIDs(auth)
	key := resolutionKey{tenantID: tenantID, authFileID: authFileID, providerKeyID: providerKeyID, providerID: providerID}
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
		m.setSnapshot(ctx, auth, resolved.source, profile, decision, false, false)
		m.logDecision(tenantID, auth, resolved.source, profile, decision, false)
		return coreauth.RequestModerationResult{}
	}

	input := ExtractModeratableText(opts.SourceFormat, opts.OriginalRequest)
	if input == "" {
		counters.allows.Add(1)
		decision := Decision{Action: ActionAllow}
		m.setSnapshot(ctx, auth, resolved.source, profile, decision, false, false)
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
		m.setSnapshot(ctx, auth, source, profile, decision, true, cached)
		m.recordError(tenantID, auth, source, profile, errorClass, decision.LatencyMS, cached)
		return coreauth.RequestModerationResult{}
	}
	m.setSnapshot(ctx, auth, source, profile, decision, true, cached)
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
	} else if decision.HighestCategory != "" {
		fields["category"] = decision.HighestCategory
		fields["score"] = decision.HighestScore
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
	return log.Fields{
		"tenant_id":         tenantID,
		"profile_id":        profile.ID,
		"profile_version":   profile.Version,
		"resolution_source": source,
		"channel_type":      channelType,
		"channel_id":        channelID,
	}
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

func moderationAPIPath(profile Profile, input string) bool {
	if profile.Mode != ModePreBlock || profile.KeywordMode == KeywordModeKeywordOnly || strings.TrimSpace(input) == "" {
		return false
	}
	if profile.KeywordMode == KeywordModeKeywordAndAPI {
		_, keywordHit := matchKeyword(input, profile.BlockedKeywords)
		return !keywordHit
	}
	return true
}

func decisionCacheKey(profile Profile, input string) string {
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s:%d:%x", profile.ID, profile.Version, hash)
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
	case strings.Contains(message, "decode") || strings.Contains(message, "missing category scores"):
		return "invalid_response"
	case strings.Contains(message, "request failed"):
		return "transport"
	default:
		return "moderation_error"
	}
}
