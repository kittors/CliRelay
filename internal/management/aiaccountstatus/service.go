package aiaccountstatus

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	defaultMaxConcurrency  = 4
	jobTTL                 = 30 * time.Minute
	accountRefreshMinGap   = 20 * time.Second
	probeTimeout           = 25 * time.Second
	staleRefreshThreshold  = 15 * time.Minute
	staleNormalizeInterval = 5 * time.Minute // GET stays read-mostly; do not UPDATE every list
)

type CacheInvalidator func(tenantID, authIndex, authSubjectID string)
type APIToolsFactory func(tenantID string) *managementapitools.Service

// ProbeFunc is injectable for tests.
type ProbeFunc func(ctx context.Context, svc *managementapitools.Service, cfg *config.Config, auth *coreauth.Auth) (ProbeResult, error)

type Service struct {
	cfg            *config.Config
	authManager    *coreauth.Manager
	apiToolsFor    APIToolsFactory
	invalidate     CacheInvalidator
	maxConcurrency int
	probeFn        ProbeFunc
	// upsertStatus is nil in production (uses usage.UpsertAIAccountStatus); tests may override.
	upsertStatus func(usage.AIAccountStatusRecord) error
	// reconcileQuota is nil in production (uses authManager.ReconcileQuota); tests may override.
	reconcileQuota func(ctx context.Context, authID string) (bool, error)
	// normalizeStale is nil in production (usage.NormalizeStaleAIAccountRefreshStates).
	normalizeStale func(tenantID string, olderThan time.Duration) (int64, error)

	sf  singleflight.Group
	sem chan struct{} // process-wide probe concurrency across jobs

	mu            sync.Mutex
	jobs          map[string]*job
	inFlight      map[string]string    // shared subject -> owning job ID (job visibility stays tenant-private)
	lastSuccess   map[string]time.Time // shared subject -> last successful probe (closes force=false TOCTOU)
	staleNormMu   sync.Mutex
	lastStaleNorm map[string]time.Time
}

type job struct {
	ID        string
	TenantID  string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Results   map[string]*AccountRefreshResult
	order     []string
}

func New(cfg *config.Config, authManager *coreauth.Manager, apiToolsFor APIToolsFactory, invalidate CacheInvalidator) *Service {
	return &Service{
		cfg:            cfg,
		authManager:    authManager,
		apiToolsFor:    apiToolsFor,
		invalidate:     invalidate,
		maxConcurrency: defaultMaxConcurrency,
		probeFn:        probeAuth,
		sem:            make(chan struct{}, defaultMaxConcurrency),
		jobs:           make(map[string]*job),
		inFlight:       make(map[string]string),
		lastSuccess:    make(map[string]time.Time),
		lastStaleNorm:  make(map[string]time.Time),
	}
}

// SetProbeFunc overrides upstream probing (tests).
func (s *Service) SetProbeFunc(fn ProbeFunc) {
	if s == nil {
		return
	}
	s.probeFn = fn
}

// SetUpsertStatusFunc overrides latest-status persistence (tests).
func (s *Service) SetUpsertStatusFunc(fn func(usage.AIAccountStatusRecord) error) {
	if s == nil {
		return
	}
	s.upsertStatus = fn
}

// SetReconcileQuotaFunc overrides runtime quota reconcile (tests).
func (s *Service) SetReconcileQuotaFunc(fn func(ctx context.Context, authID string) (bool, error)) {
	if s == nil {
		return
	}
	s.reconcileQuota = fn
}

// SetNormalizeStaleFunc overrides stale refresh normalization (tests).
func (s *Service) SetNormalizeStaleFunc(fn func(tenantID string, olderThan time.Duration) (int64, error)) {
	if s == nil {
		return
	}
	s.normalizeStale = fn
}

// SetMaxConcurrency adjusts the global probe semaphore (tests).
func (s *Service) SetMaxConcurrency(n int) {
	if s == nil || n < 1 {
		return
	}
	s.maxConcurrency = n
	s.sem = make(chan struct{}, n)
}

func (s *Service) StartRefresh(tenantID string, req RefreshRequest) RefreshAccepted {
	s.purgeExpiredJobs()
	tenantID = strings.TrimSpace(tenantID)
	// Refresh boundary: allow stale cleanup without waiting for GET throttle alone.
	s.maybeNormalizeStaleRefresh(tenantID)
	auths := s.listAuths(tenantID)
	// Best-effort: still start refresh even if some binding repairs fail.
	_ = reconcileTenantBindings(auths)
	byIndex := make(map[string]*coreauth.Auth, len(auths))
	bySubject := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		byIndex[auth.Index] = auth
		if id := usage.ResolveAuthSubjectIdentity(auth); id != nil && id.ID != "" {
			// Prefer enabled/usable credential when multiple aliases share one subject.
			bySubject[id.ID] = preferAuthRepresentative(bySubject[id.ID], auth)
		}
	}

	// subjectID -> best representative auth (prefer usable over disabled).
	picks := make(map[string]*coreauth.Auth)
	add := func(auth *coreauth.Auth) {
		if auth == nil {
			return
		}
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			return
		}
		picks[identity.ID] = preferAuthRepresentative(picks[identity.ID], auth)
	}
	for _, idx := range req.AuthIndexes {
		add(byIndex[strings.TrimSpace(idx)])
	}
	for _, sid := range req.AuthSubjectIDs {
		add(bySubject[strings.TrimSpace(sid)])
	}
	if len(req.AuthIndexes) == 0 && len(req.AuthSubjectIDs) == 0 {
		for _, auth := range auths {
			add(auth)
		}
	}

	selected := make([]*coreauth.Auth, 0, len(picks))
	subjectIDs := make([]string, 0, len(picks))
	for sid, auth := range picks {
		selected = append(selected, auth)
		subjectIDs = append(subjectIDs, sid)
	}

	// Batch-load recent success outside s.mu so GetJob/other refreshes are not blocked
	// by N small-table queries under the service lock.
	recentBySubject := map[string]time.Time{}
	if !req.Force && len(subjectIDs) > 0 {
		if rows, err := usage.ListAIAccountSubjectStatus(subjectIDs); err == nil {
			nowCheck := time.Now()
			for _, rec := range rows {
				if rec.LastProbeState != string(RefreshSuccess) || rec.UpstreamCheckedAt == nil {
					continue
				}
				if nowCheck.Sub(*rec.UpstreamCheckedAt) < accountRefreshMinGap {
					recentBySubject[rec.AuthSubjectID] = *rec.UpstreamCheckedAt
				}
			}
		}
	}

	now := time.Now().UTC()
	j := &job{
		ID:        uuid.NewString(),
		TenantID:  tenantID,
		State:     "running",
		CreatedAt: now,
		UpdatedAt: now,
		Results:   make(map[string]*AccountRefreshResult),
	}

	accepted := 0
	deduped := 0
	skipped := make([]string, 0)
	workAuths := make([]*coreauth.Auth, 0)

	s.mu.Lock()
	for _, auth := range selected {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		subjectID := identity.ID
		key := flightKey(tenantID, subjectID)

		// Disabled credentials are represented by the auth manager itself; probing
		// them only creates avoidable upstream failures and cannot make them active.
		// (Representative pick already preferred enabled aliases when any exist.)
		if !isAuthUsable(auth) {
			skipped = append(skipped, auth.Index)
			j.Results[subjectID] = &AccountRefreshResult{
				AuthIndex:     auth.Index,
				AuthSubjectID: subjectID,
				State:         RefreshSuccess,
				ErrorCode:     "disabled",
				ErrorMessage:  "skipped: account disabled",
				UpdatedAt:     now,
			}
			j.order = append(j.order, subjectID)
			continue
		}

		// force never bypasses in-flight singleflight/dedupe.
		if _, busy := s.inFlight[key]; busy {
			deduped++
			j.Results[subjectID] = &AccountRefreshResult{
				AuthIndex:     auth.Index,
				AuthSubjectID: subjectID,
				State:         RefreshError,
				ErrorCode:     "deduplicated",
				ErrorMessage:  "shared refresh already in progress",
				UpdatedAt:     now,
			}
			j.order = append(j.order, subjectID)
			continue
		}

		if !req.Force {
			// Combine lock-external DB batch with lock-local success memory so a
			// probe that finished after the batch read still respects min-gap.
			recent, ok := recentBySubject[subjectID]
			if mem, memOK := s.lastSuccess[key]; memOK && (now.Sub(mem) < accountRefreshMinGap) {
				if !ok || mem.After(recent) {
					recent, ok = mem, true
				}
			}
			if ok {
				skipped = append(skipped, auth.Index)
				j.Results[subjectID] = &AccountRefreshResult{
					AuthIndex:     auth.Index,
					AuthSubjectID: subjectID,
					State:         RefreshSuccess,
					ErrorCode:     "fresh",
					ErrorMessage:  "skipped: recently refreshed",
					UpdatedAt:     recent,
				}
				j.order = append(j.order, subjectID)
				continue
			}
		}

		s.inFlight[key] = j.ID
		j.Results[subjectID] = &AccountRefreshResult{
			AuthIndex:     auth.Index,
			AuthSubjectID: subjectID,
			State:         RefreshQueued,
			UpdatedAt:     now,
		}
		j.order = append(j.order, subjectID)
		workAuths = append(workAuths, auth)
		accepted++
	}
	// Job with only terminal items is already completed.
	if accepted == 0 {
		j.State = "completed"
	}
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Persist queued only for truly accepted accounts (partial update).
	for _, auth := range workAuths {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil {
			continue
		}
		_ = usage.UpdateAIAccountRefreshState(tenantID, identity.ID, auth.Index, auth.Provider, string(RefreshQueued), string(auth.Status), "", "")
	}

	if accepted > 0 {
		go s.runJob(j.ID, tenantID, workAuths)
	}

	return RefreshAccepted{
		JobID:        j.ID,
		Accepted:     accepted,
		Deduplicated: deduped,
		Skipped:      skipped,
	}
}

func isAuthUsable(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return !auth.Disabled && auth.Status != coreauth.StatusDisabled
}

// needsRuntimeQuotaReconcile reports whether local scheduler state has an active
// quota cooldown that still needs ProbeQuotaRecovery. Normal accounts return false
// so status refresh performs only the management probe (one upstream call).
func needsRuntimeQuotaReconcile(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	now := time.Now()
	if auth.Quota.Exceeded && auth.Quota.RecoveryRequired {
		return true
	}
	if auth.Unavailable && auth.Quota.Exceeded && auth.NextRetryAfter.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Quota.Exceeded && state.Quota.RecoveryRequired {
			return true
		}
		if state.Unavailable && state.Quota.Exceeded && state.NextRetryAfter.After(now) {
			return true
		}
	}
	return false
}

func (s *Service) maybeNormalizeStaleRefresh(tenantID string) {
	if s == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	now := time.Now()
	s.staleNormMu.Lock()
	if last, ok := s.lastStaleNorm[tenantID]; ok && now.Sub(last) < staleNormalizeInterval {
		s.staleNormMu.Unlock()
		return
	}
	s.lastStaleNorm[tenantID] = now
	s.staleNormMu.Unlock()

	fn := s.normalizeStale
	if fn == nil {
		fn = usage.NormalizeStaleAIAccountRefreshStates
	}
	_, _ = fn(tenantID, staleRefreshThreshold)
}

// preferAuthRepresentative keeps an enabled credential over a disabled alias for the same subject.
func preferAuthRepresentative(current, candidate *coreauth.Auth) *coreauth.Auth {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if !isAuthUsable(current) && isAuthUsable(candidate) {
		return candidate
	}
	return current
}

func (s *Service) runJob(jobID, tenantID string, auths []*coreauth.Auth) {
	var wg sync.WaitGroup
	for _, auth := range auths {
		auth := auth
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			continue
		}
		subjectID := identity.ID
		key := flightKey(tenantID, subjectID)

		wg.Add(1)
		go func() {
			defer wg.Done()
			// Global semaphore across all jobs.
			s.sem <- struct{}{}
			defer func() { <-s.sem }()

			_, _, _ = s.sf.Do(key, func() (any, error) {
				s.refreshOne(jobID, tenantID, auth, subjectID)
				return nil, nil
			})

			s.mu.Lock()
			if s.inFlight[key] == jobID {
				delete(s.inFlight, key)
			}
			// Record success under the same lock so force=false cannot TOCTOU-miss
			// a probe that finished between lock-external DB batch and lock acquire.
			if j := s.jobs[jobID]; j != nil {
				if r := j.Results[subjectID]; r != nil && r.State == RefreshSuccess && r.ErrorCode == "" {
					s.lastSuccess[key] = r.UpdatedAt
					if s.lastSuccess[key].IsZero() {
						s.lastSuccess[key] = time.Now().UTC()
					}
				}
			}
			s.mu.Unlock()
		}()
	}
	wg.Wait()

	s.mu.Lock()
	if j := s.jobs[jobID]; j != nil {
		j.State = "completed"
		j.UpdatedAt = time.Now().UTC()
	}
	s.mu.Unlock()
}

func (s *Service) refreshOne(jobID, tenantID string, auth *coreauth.Auth, subjectID string) {
	now := time.Now().UTC()
	s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
		r.State = RefreshRunning
		r.UpdatedAt = now
	})
	_ = usage.UpdateAIAccountRefreshState(tenantID, subjectID, auth.Index, auth.Provider, string(RefreshRunning), string(auth.Status), "", "")

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	// Status probe already hits the provider quota endpoint for codex/gemini-cli/etc.
	// Only run ReconcileQuota when the credential has an active runtime quota cooldown,
	// so normal refreshes do not double-call the same upstream URL.
	if needsRuntimeQuotaReconcile(auth) {
		if s.reconcileQuota != nil {
			_, _ = s.reconcileQuota(ctx, auth.ID)
		} else if s.authManager != nil && strings.TrimSpace(auth.ID) != "" {
			_, _ = s.authManager.ReconcileQuota(ctx, auth.ID)
		}
	}

	var probe ProbeResult
	var probeErr error
	var apiTools *managementapitools.Service
	if s.apiToolsFor != nil {
		apiTools = s.apiToolsFor(tenantID)
	}
	probeFn := s.probeFn
	if probeFn == nil {
		probeFn = probeAuth
	}
	if apiTools != nil {
		probe, probeErr = probeFn(ctx, apiTools, s.cfg, auth)
	} else {
		probeErr = fmt.Errorf("api tools unavailable")
	}

	checked := time.Now().UTC()
	if probeErr == nil {
		asserted := readProviderAssertedAccountFacts(auth)
		if strings.TrimSpace(probe.PlanType) == "" {
			probe.PlanType = asserted.PlanType
		}
		probe.SubscriptionStartedAt = asserted.SubscriptionStarted
		probe.SubscriptionExpiresAt = asserted.SubscriptionExpires
		probe.SubscriptionSource = asserted.SubscriptionSource
	}
	if probeErr != nil {
		// Probe outcomes were previously invisible in logs, so "the panel shows a
		// week-old number" could not be traced to the upstream failure behind it.
		log.WithFields(log.Fields{
			"auth_subject_id": subjectID,
			"provider":        auth.Provider,
			"auth_index":      auth.Index,
		}).WithError(probeErr).Warn("ai account status probe failed; stored quota is kept but not refreshed")
		_ = usage.UpdateAIAccountSubjectProbeFailure(subjectID, auth.Provider, "probe_failed", probeErr.Error(), checked)
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"probe_failed", probeErr.Error(), checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "probe_failed"
			r.ErrorMessage = sanitizeMsg(probeErr.Error())
			r.UpdatedAt = checked
		})
		return
	}

	if probe.Unsupported {
		_ = usage.UpdateAIAccountSubjectProbeFailure(subjectID, auth.Provider, "unsupported_probe", probe.UnsupportedReason, checked)
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"unsupported_probe", probe.UnsupportedReason, checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "unsupported_probe"
			r.ErrorMessage = sanitizeMsg(probe.UnsupportedReason)
			r.UpdatedAt = checked
		})
		return
	}
	// A 200 that parses to zero windows means the upstream payload shape changed or
	// the account genuinely has no quota. Either way the stored windows are carried
	// forward unrefreshed, so surface it instead of silently serving aging values.
	if len(probe.Quotas) == 0 {
		log.WithFields(log.Fields{
			"auth_subject_id": subjectID,
			"provider":        auth.Provider,
			"auth_index":      auth.Index,
		}).Warn("ai account status probe returned no quota windows; stored quota is kept but not refreshed")
	}
	s.applyRuntimeQuotaProbe(ctx, auth, probe)

	healthStatus := strings.TrimSpace(probe.Health)
	if healthStatus == "" {
		healthStatus = "ok"
	}
	record := usage.AIAccountStatusRecord{
		TenantID:               tenantID,
		AuthSubjectID:          subjectID,
		AuthIndex:              auth.Index,
		Provider:               auth.Provider,
		RefreshState:           string(RefreshSuccess),
		HealthStatus:           healthStatus,
		PlanType:               strings.TrimSpace(probe.PlanType),
		Quotas:                 probe.Quotas,
		ResetCreditCount:       probe.ResetCreditCount,
		ResetCreditExpirations: probe.ResetCreditExpirations,
		UpstreamCheckedAt:      &checked,
		ExpiresAt:              probe.SubscriptionExpiresAt,
		UpdatedAt:              checked,
	}
	if len(probe.Quotas) > 0 {
		daily := make(map[string]*float64, len(probe.Quotas))
		points := make([]usage.QuotaSnapshotPoint, 0, len(probe.Quotas))
		for _, q := range probe.Quotas {
			if q.Percent != nil {
				p := *q.Percent
				daily[q.QuotaKey] = &p
			}
			points = append(points, usage.QuotaSnapshotPoint{
				RecordedAt:    checked,
				AuthIndex:     auth.Index,
				Provider:      auth.Provider,
				QuotaKey:      q.QuotaKey,
				QuotaLabel:    q.QuotaLabel,
				Percent:       q.Percent,
				ResetAt:       q.ResetAt,
				WindowSeconds: q.WindowSeconds,
			})
		}
		_ = usage.RecordDailyQuotaSnapshotIdentityForTenant(tenantID, auth.Index, subjectID, auth.Provider, daily)
		_ = usage.RecordQuotaSnapshotPointsIdentityForTenant(tenantID, auth.Index, subjectID, auth.Provider, points)
		if err := usage.RecordAIAccountSubjectQuotaPoints(subjectID, auth.Provider, points); err != nil {
			_ = usage.UpdateAIAccountSubjectProbeFailure(subjectID, auth.Provider, "persist_failed", err.Error(), checked)
			s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
				r.State = RefreshError
				r.ErrorCode = "persist_failed"
				r.ErrorMessage = sanitizeMsg(err.Error())
				r.UpdatedAt = checked
			})
			return
		}
	}

	// Persist the shared runtime truth before the legacy tenant mirror. Shared
	// status/version must not depend on a legacy reload succeeding.
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: subjectID, Provider: auth.Provider, LastProbeState: string(RefreshSuccess),
		HealthStatus: healthStatus, PlanType: strings.TrimSpace(probe.PlanType),
		SubscriptionStartedAt: probe.SubscriptionStartedAt, SubscriptionExpiresAt: probe.SubscriptionExpiresAt,
		SubscriptionSource: probe.SubscriptionSource, RestrictionSummary: record.RestrictionSummary, Quotas: probe.Quotas,
		ResetCreditCount: probe.ResetCreditCount, ResetCreditExpirations: probe.ResetCreditExpirations,
		UpstreamCheckedAt: &checked, UpdatedAt: checked,
	}); err != nil {
		_ = usage.UpdateAIAccountRefreshFailure(tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status), "persist_failed", err.Error(), checked)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_failed"
			r.ErrorMessage = sanitizeMsg(err.Error())
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}

	// Keep the legacy tenant mirror for compatibility while shared remains authoritative.
	upsert := s.upsertStatus
	if upsert == nil {
		upsert = usage.UpsertAIAccountStatus
	}
	if err := upsert(record); err != nil {
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"persist_failed", err.Error(), checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_failed"
			r.ErrorMessage = sanitizeMsg(err.Error())
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}
	legacyPersisted, err := s.loadLegacyPersistedStatus(tenantID, subjectID)
	if err != nil || legacyPersisted == nil {
		msg := "persisted status reload failed"
		if err != nil {
			msg = err.Error()
		}
		_ = usage.UpdateAIAccountRefreshFailure(tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status), "persist_reload_failed", msg, checked)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_reload_failed"
			r.ErrorMessage = sanitizeMsg(msg)
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}
	// DB assigns version = previous+1 on conflict; never invent version from the in-memory draft.
	persisted, err := s.loadPersistedStatus(subjectID)
	if err != nil || persisted == nil {
		msg := "persisted status reload failed"
		if err != nil {
			msg = err.Error()
		}
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"persist_reload_failed", msg, checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_reload_failed"
			r.ErrorMessage = sanitizeMsg(msg)
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}
	if s.invalidate != nil {
		s.invalidate(tenantID, auth.Index, subjectID)
	}

	// Progressive view uses DB-truth version/updated fields for frontend monotonic merge.
	view := s.viewFromPersistedRecord(tenantID, auth, *persisted)
	s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
		r.State = RefreshSuccess
		r.ErrorCode = ""
		r.ErrorMessage = ""
		r.UpdatedAt = checked
		r.Result = view
	})
}

func (s *Service) applyRuntimeQuotaProbe(ctx context.Context, auth *coreauth.Auth, probe ProbeResult) {
	if s == nil || s.authManager == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "xai" && provider != "x-ai" && provider != "grok" {
		return
	}
	for _, quota := range probe.Quotas {
		if quota.QuotaKey != "weekly_limit" || quota.Percent == nil {
			continue
		}
		// Same threshold the runtime recovery probe uses: percentages now carry
		// upstream precision, and a fraction of a percent left cannot serve a
		// request. Calling that recovered puts the credential back in rotation to
		// take another 402 and cool down again, over and over until the reset.
		exhausted := xaiauth.WeeklyBilling{RemainingPercent: *quota.Percent}.Exhausted()
		result := &coreauth.QuotaProbeResult{
			Recovered:       !exhausted,
			WindowExhausted: exhausted,
			Window:          "week",
			WindowMinutes:   10080,
		}
		if quota.ResetAt != nil && quota.ResetAt.After(time.Now()) {
			result.NextRecoverAt = *quota.ResetAt
		}
		_, _ = s.authManager.UpdateQuotaFromProbe(ctx, auth.ID, result)
		return
	}
}

func (s *Service) loadLegacyPersistedStatus(tenantID, subjectID string) (*usage.AIAccountStatusRecord, error) {
	rows, err := usage.ListAIAccountStatusForTenant(tenantID, []string{subjectID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("status row missing after upsert")
	}
	rec := rows[0]
	return &rec, nil
}

func (s *Service) loadPersistedStatus(subjectID string) (*usage.AIAccountSubjectStatusRecord, error) {
	rows, err := usage.ListAIAccountSubjectStatus([]string{subjectID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("status row missing after upsert")
	}
	rec := rows[0]
	return &rec, nil
}

func (s *Service) viewFromPersistedRecord(tenantID string, auth *coreauth.Auth, record usage.AIAccountSubjectStatusRecord) *AccountStatusView {
	cycleStarts, _ := usage.QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{record.AuthSubjectID})
	summaries, _ := usage.QueryAIAccountSubjectUsageSummaries([]string{record.AuthSubjectID}, cycleStarts)
	subjects, _ := usage.ListAIAccountSubjects([]string{record.AuthSubjectID})
	counts, _ := usage.CountAIAccountTenantBindings(tenantID, []string{record.AuthSubjectID})
	view := statusViewFromSharedRecord(record, auth, summaries[record.AuthSubjectID], subjects[record.AuthSubjectID], counts[record.AuthSubjectID])
	return &view
}

func (s *Service) setResult(jobID, subjectID string, fn func(*AccountRefreshResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil {
		return
	}
	r := j.Results[subjectID]
	if r == nil {
		r = &AccountRefreshResult{AuthSubjectID: subjectID}
		j.Results[subjectID] = r
	}
	fn(r)
	j.UpdatedAt = time.Now().UTC()
}

func (s *Service) GetJob(tenantID, jobID string) (JobSnapshot, bool) {
	s.purgeExpiredJobs()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil || j.TenantID != strings.TrimSpace(tenantID) {
		return JobSnapshot{}, false
	}
	snap := JobSnapshot{
		JobID:     j.ID,
		TenantID:  j.TenantID,
		State:     j.State,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
		Results:   make([]AccountRefreshResult, 0, len(j.order)),
	}
	for _, sid := range j.order {
		r := j.Results[sid]
		if r == nil {
			continue
		}
		snap.Results = append(snap.Results, *r)
		snap.Total++
		switch {
		case r.ErrorCode == "deduplicated" || r.ErrorCode == "fresh":
			snap.Completed++
		case r.State == RefreshSuccess:
			snap.Completed++
		case r.State == RefreshError:
			snap.Failed++
			snap.Completed++
		}
	}
	return snap, true
}

func (s *Service) ListStatus(tenantID string, authIndexes, authSubjectIDs []string) (StatusListResponse, error) {
	tenantID = strings.TrimSpace(tenantID)
	s.maybeNormalizeStaleRefresh(tenantID)
	auths := s.listAuths(tenantID)
	// Binding reconcile is best-effort; never fail the read path for legacy write noise.
	_ = reconcileTenantBindings(auths)
	indexFilter := make(map[string]struct{})
	for _, idx := range authIndexes {
		if v := strings.TrimSpace(idx); v != "" {
			indexFilter[v] = struct{}{}
		}
	}
	subjectFilter := make(map[string]struct{})
	for _, sid := range authSubjectIDs {
		if v := strings.TrimSpace(sid); v != "" {
			subjectFilter[v] = struct{}{}
		}
	}
	filterOn := len(indexFilter) > 0 || len(subjectFilter) > 0
	wanted := make(map[string]*coreauth.Auth)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			continue
		}
		if filterOn {
			_, okIdx := indexFilter[auth.Index]
			_, okSub := subjectFilter[identity.ID]
			if !okIdx && !okSub {
				continue
			}
		}
		wanted[identity.ID] = preferAuthRepresentative(wanted[identity.ID], auth)
	}
	subjectIDs := make([]string, 0, len(wanted))
	for sid := range wanted {
		subjectIDs = append(subjectIDs, sid)
	}
	if len(subjectIDs) == 0 {
		return StatusListResponse{Items: []AccountStatusView{}}, nil
	}

	statusRows, err := usage.ListAIAccountSubjectStatus(subjectIDs)
	if err != nil {
		return StatusListResponse{}, err
	}
	statusBySubject := make(map[string]usage.AIAccountSubjectStatusRecord, len(statusRows))
	for _, row := range statusRows {
		statusBySubject[row.AuthSubjectID] = row
	}
	subjectRows, err := usage.ListAIAccountSubjects(subjectIDs)
	if err != nil {
		return StatusListResponse{}, err
	}
	bindingCounts, err := usage.CountAIAccountTenantBindings(tenantID, subjectIDs)
	if err != nil {
		return StatusListResponse{}, err
	}
	cycleStart, err := usage.QueryLatestAIAccountSubjectWeeklyCyclesBatch(subjectIDs)
	if err != nil {
		return StatusListResponse{}, err
	}
	summaries, err := usage.QueryAIAccountSubjectUsageSummaries(subjectIDs, cycleStart)
	if err != nil {
		return StatusListResponse{}, err
	}

	items := make([]AccountStatusView, 0, len(subjectIDs))
	for _, sid := range subjectIDs {
		auth := wanted[sid]
		identity := usage.ResolveAuthSubjectIdentity(auth)
		subject := subjectRows[sid]
		if subject.AuthSubjectID == "" && identity != nil {
			subject = usage.AIAccountSubjectRecord{AuthSubjectID: sid, Provider: identity.Provider, SubjectScope: identity.SubjectScope, SeedKind: identity.SeedKind, ShareEligible: identity.ShareEligible}
		}
		row, has := statusBySubject[sid]
		if !has {
			items = append(items, AccountStatusView{
				AuthSubjectID: sid, AuthIndex: auth.Index, Provider: auth.Provider,
				StatusScope: usage.AIAccountStatusScopeShared, SubjectScope: subject.SubjectScope,
				ShareEligible: subject.ShareEligible, SubjectSeedKind: subject.SeedKind,
				CurrentTenantBindingCount: bindingCounts[sid], RefreshState: string(RefreshIdle),
				HealthStatus: string(auth.Status), Quotas: []usage.QuotaWindowDTO{}, Usage: summaries[sid],
			})
			continue
		}
		items = append(items, statusViewFromSharedRecord(row, auth, summaries[sid], subject, bindingCounts[sid]))
	}
	return StatusListResponse{Items: items}, nil
}

func (s *Service) listAuths(tenantID string) []*coreauth.Auth {
	if s == nil || s.authManager == nil {
		return nil
	}
	return s.authManager.ListForTenant(tenantID)
}

func (s *Service) purgeExpiredJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Drop stale success memory outside min-gap so the map stays bounded.
	for key, at := range s.lastSuccess {
		if now.Sub(at) >= accountRefreshMinGap {
			delete(s.lastSuccess, key)
		}
	}
	for id, j := range s.jobs {
		if now.Sub(j.UpdatedAt) > jobTTL {
			delete(s.jobs, id)
		}
	}
	for key, jobID := range s.inFlight {
		if _, ok := s.jobs[jobID]; !ok {
			delete(s.inFlight, key)
		}
	}
}

func flightKey(_ string, subjectID string) string {
	return strings.TrimSpace(subjectID)
}

func reconcileTenantBindings(auths []*coreauth.Auth) error {
	if len(auths) == 0 {
		return nil
	}
	tenantID := ""
	authIDs := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			tenantID = auth.TenantID
			if id := strings.TrimSpace(auth.ID); id != "" {
				authIDs = append(authIDs, id)
			}
		}
	}
	rows, err := usage.ListAIAccountBindingsForTenantAuths(tenantID, authIDs)
	if err != nil {
		return err
	}
	byID := make(map[string]usage.AIAccountTenantBinding, len(rows))
	for _, row := range rows {
		byID[row.AuthID] = row
	}
	// Best-effort per auth: one bad binding row must not 500 the whole status list.
	var firstErr error
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil {
			continue
		}
		row, ok := byID[auth.ID]
		if ok && row.BindingState == "active" && row.AuthSubjectID == identity.ID && row.AuthIndex == auth.EnsureIndex() && row.BindingSeedHash == identity.SeedHash {
			continue
		}
		if err := usage.UpsertAIAccountTenantBinding(auth, identity); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func sanitizeMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return "upstream request failed"
	}
	if len(msg) > 200 {
		return msg[:200]
	}
	return msg
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func metadataString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := auth.Metadata[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}
