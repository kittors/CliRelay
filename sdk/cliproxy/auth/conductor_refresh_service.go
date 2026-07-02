package auth

import (
	"context"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
)

type refreshService struct {
	manager *Manager
}

func newRefreshService(manager *Manager) refreshService {
	return refreshService{manager: manager}
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	newRefreshService(m).startAutoRefresh(parent, interval)
}

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	newRefreshService(m).stopAutoRefresh()
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	newRefreshService(m).checkRefreshes(ctx)
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	newRefreshService(m).refreshAuthWithLimit(ctx, id)
}

func (m *Manager) snapshotAuths() []*Auth {
	return newRefreshService(m).snapshotAuths()
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	return newRefreshService(m).shouldRefresh(a, now)
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	return newRefreshService(m).markRefreshPending(id, now)
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	newRefreshService(m).refreshAuth(ctx, id)
}

func (m *Manager) recoverRotatedRefreshToken(ctx context.Context, id string, used *Auth, now time.Time, refreshErr error) bool {
	return newRefreshService(m).recoverRotatedRefreshToken(ctx, id, used, now, refreshErr)
}

func (s refreshService) startAutoRefresh(parent context.Context, interval time.Duration) {
	if s.manager == nil {
		return
	}
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	if s.manager.refreshCancel != nil {
		s.manager.refreshCancel()
		s.manager.refreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	s.manager.refreshCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.checkRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkRefreshes(ctx)
			}
		}
	}()
}

func (s refreshService) stopAutoRefresh() {
	if s.manager == nil {
		return
	}
	if s.manager.refreshCancel != nil {
		s.manager.refreshCancel()
		s.manager.refreshCancel = nil
	}
}

func (s refreshService) checkRefreshes(ctx context.Context) {
	if s.manager == nil {
		return
	}
	now := time.Now()
	snapshot := s.snapshotAuths()
	for _, auth := range snapshot {
		typ, _ := auth.AccountInfo()
		if typ == "api_key" {
			continue
		}
		if !s.shouldRefresh(auth, now) {
			continue
		}
		log.Debugf("checking refresh for %s, %s, %s", auth.Provider, auth.ID, typ)

		if exec := s.manager.executorFor(auth.Provider); exec == nil {
			continue
		}
		if !s.markRefreshPending(auth.ID, now) {
			continue
		}
		go s.refreshAuthWithLimit(ctx, auth.ID)
	}
	s.manager.checkQuotaRecoveries(ctx, snapshot, now)
}

func (s refreshService) refreshAuthWithLimit(ctx context.Context, id string) {
	if s.manager == nil {
		return
	}
	if s.manager.refreshSemaphore == nil {
		s.refreshAuth(ctx, id)
		return
	}
	select {
	case s.manager.refreshSemaphore <- struct{}{}:
		defer func() { <-s.manager.refreshSemaphore }()
	case <-ctx.Done():
		return
	}
	s.refreshAuth(ctx, id)
}

func (s refreshService) snapshotAuths() []*Auth {
	if s.manager == nil {
		return nil
	}
	s.manager.mu.RLock()
	defer s.manager.mu.RUnlock()
	out := make([]*Auth, 0, len(s.manager.auths))
	for _, auth := range s.manager.auths {
		out = append(out, auth.Clone())
	}
	return out
}

func (s refreshService) shouldRefresh(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := auth.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, auth)
	}
	if claudeOAuthRefreshPending(auth) {
		return true
	}

	lastRefresh := auth.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(auth); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := auth.ExpirationTime()

	if interval := authPreferredInterval(auth); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	lead := ProviderRefreshLead(auth.Provider, auth.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func (s refreshService) markRefreshPending(id string, now time.Time) bool {
	if s.manager == nil {
		return false
	}
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	auth, ok := s.manager.auths[id]
	if !ok || auth == nil {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	s.manager.auths[id] = auth
	return true
}

func (s refreshService) refreshAuth(ctx context.Context, id string) {
	if s.manager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auth, exec := s.currentAuthAndExecutor(id)
	if auth == nil || exec == nil {
		return
	}
	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		s.applyRefreshFailure(ctx, id, auth, cloned, now, err)
		return
	}
	s.applyRefreshSuccess(ctx, auth, cloned, updated, now)
}

func (s refreshService) currentAuthAndExecutor(id string) (*Auth, ProviderExecutor) {
	if s.manager == nil {
		return nil, nil
	}
	s.manager.mu.RLock()
	defer s.manager.mu.RUnlock()
	auth := s.manager.auths[id]
	if auth == nil {
		return nil, nil
	}
	return auth, s.manager.executors[auth.Provider]
}

func (s refreshService) applyRefreshFailure(ctx context.Context, id string, auth *Auth, cloned *Auth, now time.Time, err error) {
	permanent := IsPermanentAuthError(err)
	if permanent {
		log.Warnf("permanent refresh failure for %s (%s): %v", auth.ID, auth.Provider, err)
		if supportsRefreshTokenRaceRecovery(cloned) && s.recoverRotatedRefreshToken(ctx, id, cloned, now, err) {
			return
		}
	}
	s.manager.mu.Lock()
	if current := s.manager.auths[id]; current != nil {
		current.NextRefreshAfter = now.Add(refreshFailureBackoff)
		current.LastError = &Error{Message: err.Error()}
		if permanent {
			if supportsRefreshTokenRaceRecovery(current) && canKeepRefreshFailureActive(current, now) {
				current.Status = StatusActive
				current.StatusMessage = ""
			} else {
				current.Status = StatusError
				current.StatusMessage = err.Error()
			}
		}
		current.UpdatedAt = now
		s.manager.auths[id] = current
	}
	s.manager.mu.Unlock()
}

func (s refreshService) applyRefreshSuccess(ctx context.Context, auth *Auth, cloned *Auth, updated *Auth, now time.Time) {
	if updated == nil {
		updated = cloned
	}
	if updated.Runtime == nil && auth != nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	markClaudeOAuthHealthRefreshSuccessLocked(updated, now)
	_, _ = s.manager.Update(ctx, updated)
}

func (s refreshService) recoverRotatedRefreshToken(ctx context.Context, id string, used *Auth, now time.Time, refreshErr error) bool {
	if s.manager == nil {
		return false
	}
	usedRefreshToken := authRefreshToken(used)
	if usedRefreshToken == "" {
		return false
	}

	latest := s.findAuthWithDifferentRefreshToken(id, usedRefreshToken)
	if latest == nil {
		if items, err := s.manager.loadPersistedAuths(ctx); err == nil {
			for _, item := range items {
				if item == nil || item.ID != id {
					continue
				}
				if latestRefreshToken := authRefreshToken(item); latestRefreshToken != "" && latestRefreshToken != usedRefreshToken {
					latest = item.Clone()
				}
				break
			}
		} else if s.manager.store != nil {
			log.Debugf("failed to re-read auth store after refresh failure for %s: %v", id, err)
		}
	}
	if latest == nil {
		return false
	}

	s.manager.mu.Lock()
	if current := s.manager.auths[id]; current != nil {
		if currentRefreshToken := authRefreshToken(current); currentRefreshToken != "" && currentRefreshToken != usedRefreshToken {
			latest = current.Clone()
		} else {
			preserveRuntimeFields(latest, current)
		}
	} else {
		s.manager.mu.Unlock()
		return false
	}
	applyRecoveredRefreshState(latest, now, refreshErr)
	latest.UpdatedAt = now
	latest.EnsureIndex()
	s.manager.auths[id] = latest.Clone()
	s.manager.mu.Unlock()

	log.Infof("recovered refresh failure for %s by loading rotated refresh token", id)
	s.manager.hook.OnAuthUpdated(ctx, latest.Clone())
	return true
}

func (s refreshService) findAuthWithDifferentRefreshToken(id string, usedRefreshToken string) *Auth {
	if s.manager == nil {
		return nil
	}
	s.manager.mu.RLock()
	defer s.manager.mu.RUnlock()
	current := s.manager.auths[id]
	if current == nil {
		return nil
	}
	currentRefreshToken := authRefreshToken(current)
	if currentRefreshToken == "" || currentRefreshToken == usedRefreshToken {
		return nil
	}
	return current.Clone()
}
