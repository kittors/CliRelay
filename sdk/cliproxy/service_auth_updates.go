package cliproxy

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan runtimeAuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	s.authQueueWG.Add(1)
	go func() {
		defer s.authQueueWG.Done()
		s.consumeAuthUpdates(queueCtx)
	}()
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			s.handleAuthUpdate(ctx, update)
		labelDrain:
			for {
				select {
				case nextUpdate := <-s.authUpdates:
					s.handleAuthUpdate(ctx, nextUpdate)
				default:
					break labelDrain
				}
			}
		}
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update runtimeAuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		// Service-originated updates (for example websocket provider lifecycle events)
		// may be emitted outside any request scope. In that case the service owns a
		// detached root context and the downstream auth-update queue provides the
		// bounded handoff/cleanup behavior.
		ctx = context.Background()
	}
	if s.watcher != nil && s.watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update runtimeAuthUpdate) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return
	}
	switch update.Action {
	case runtimeAuthUpdateActionAdd, runtimeAuthUpdateActionModify:
		if update.Auth == nil || update.Auth.ID == "" {
			return
		}
		s.applyCoreAuthAddOrUpdate(ctx, update.Auth)
	case runtimeAuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id == "" {
			return
		}
		s.applyCoreAuthRemoval(ctx, id)
	default:
		log.Debugf("received unknown auth update action: %v", update.Action)
	}
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	auth = auth.Clone()
	s.ensureExecutorsForAuth(auth)

	// IMPORTANT: Update coreManager FIRST, before model registration.
	// This ensures that configuration changes (proxy_url, prefix, etc.) take effect
	// immediately for API calls, rather than waiting for model registration to complete.
	// Model registration may involve network calls (e.g., FetchAntigravityModels) that
	// could timeout if the new proxy_url is unreachable.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		if s.coreManager.CredentialsVersioned() && !acceptCredentialReload(auth, existing) {
			return
		}
		auth.CreatedAt = existing.CreatedAt
		auth.LastRefreshedAt = existing.LastRefreshedAt
		auth.NextRefreshAfter = existing.NextRefreshAfter
		op = "update"
		_, err = s.coreManager.Update(ctx, auth)
	} else {
		_, err = s.coreManager.Register(ctx, auth)
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			return
		}
		auth = current
	}

	// Register models after auth is updated in coreManager.
	// This operation may block on network calls, but the auth configuration
	// is already effective at this point.
	s.registerModelsForAuth(ctx, auth)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || id == "" {
		return
	}
	if s.coreManager == nil {
		return
	}
	if _, err := s.coreManager.Delete(coreauth.WithSkipPersist(ctx), id); err != nil {
		log.Errorf("failed to remove auth %s: %v", id, err)
		return
	}
	GlobalModelRegistry().UnregisterClient(id)
}

// acceptCredentialReload guards reloads of a credential that is already in
// memory. In cluster mode auth files are mirrors of the database stamped with
// a version; the watcher can deliver a copy older than one this node already
// adopted (a slow event, or a stray file without a stamp), and applying it
// would roll the credential back. An unchanged version keeps this node's own
// runtime observations instead of the file's snapshot of them. It only runs
// with a versioned store: mirror files left behind after switching back to
// a single node still carry stamps, and must not make edits be ignored.
func acceptCredentialReload(incoming, existing *coreauth.Auth) bool {
	current := coreauth.CredentialVersion(existing)
	if current <= 0 {
		return true
	}
	version := coreauth.CredentialVersion(incoming)
	if version < current {
		log.Debugf("ignoring stale reload of auth %s (version %d, have %d)", incoming.ID, version, current)
		return false
	}
	if version == current {
		coreauth.PreserveRuntimeMetadata(incoming, existing)
	}
	return true
}
