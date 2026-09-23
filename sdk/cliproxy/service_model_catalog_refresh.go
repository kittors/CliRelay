package cliproxy

import (
	"context"
	"sync"

	log "github.com/sirupsen/logrus"
)

// catalogRefreshState coalesces model-library driven re-registrations per tenant.
//
// Re-registration re-reads the library and, for live-discovery providers, talks
// to the upstream. That is far too slow to run inline on the management save
// request, and a burst of edits must not fan out into a burst of upstream calls:
// while a tenant refresh is running, further changes only set a "run once more"
// flag, so every edit is still reflected by the final pass.
type catalogRefreshState struct {
	mu      sync.Mutex
	running map[string]bool
	pending map[string]bool
}

// begin reports whether the caller should start a refresh loop for tenantID.
func (c *catalogRefreshState) begin(tenantID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running == nil {
		c.running = make(map[string]bool)
		c.pending = make(map[string]bool)
	}
	if c.running[tenantID] {
		c.pending[tenantID] = true
		return false
	}
	c.running[tenantID] = true
	return true
}

// finish reports whether another pass is required because changes landed while
// the previous one was running.
func (c *catalogRefreshState) finish(tenantID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[tenantID] {
		delete(c.pending, tenantID)
		return true
	}
	delete(c.running, tenantID)
	return false
}

// abandon forgets a refresh loop that exits without finishing, so the next change
// for the key starts a new one instead of only marking a pass as pending.
func (c *catalogRefreshState) abandon(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	delete(c.running, key)
}

func (s *Service) onModelCatalogChanged(tenantID string) {
	if s == nil || s.coreManager == nil {
		return
	}
	if !s.catalogRefresh.begin(tenantID) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("model catalog refresh panicked: tenant=%q err=%v", tenantID, r)
				s.catalogRefresh.finish(tenantID)
			}
		}()
		for {
			s.refreshRegisteredModelsForTenant(context.Background(), tenantID)
			if !s.catalogRefresh.finish(tenantID) {
				return
			}
		}
	}()
}

// refreshRegisteredModelsForTenant re-registers every credential of one tenant.
// An empty tenant id means the system tenant, which is how single-tenant
// deployments and CLI usage store their credentials.
func (s *Service) refreshRegisteredModelsForTenant(ctx context.Context, tenantID string) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, auth := range s.coreManager.ListForTenant(tenantID) {
		s.registerModelsForAuth(ctx, auth)
	}
}
