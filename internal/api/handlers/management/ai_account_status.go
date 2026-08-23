package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// invalidateAIAccountCaches drops auth-file-trend keys for every credential
// alias that shares the same canonical auth_subject_id in this tenant.
// Dashboard summary, entity-stats, and group trend are request_logs aggregates
// and must not be cleared when quota/status updates.
func (h *Handler) invalidateAIAccountCaches(tenantID, authIndex, authSubjectID string) {
	if h == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	authIndex = strings.TrimSpace(authIndex)
	authSubjectID = strings.TrimSpace(authSubjectID)

	indexes := make(map[string]struct{})
	if authIndex != "" {
		indexes[authIndex] = struct{}{}
	}
	// Expand to all aliases of the same upstream account so detail cards stay coherent.
	if authSubjectID != "" && h.authManager != nil {
		for _, auth := range h.authManager.ListForTenant(tenantID) {
			if auth == nil {
				continue
			}
			identity := usage.ResolveAuthSubjectIdentity(auth)
			if identity == nil || identity.ID != authSubjectID {
				continue
			}
			if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
				indexes[idx] = struct{}{}
			}
		}
	}
	if len(indexes) == 0 {
		return
	}

	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	for idx := range indexes {
		prefix := "auth-file-trend:" + tenantID + ":" + idx + ":"
		for key := range h.trendCache {
			if strings.HasPrefix(key, prefix) {
				delete(h.trendCache, key)
			}
		}
	}
}

func (h *Handler) aiAccountStatusService() *aiaccountstatus.Service {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.aiAccountStatus == nil {
		h.aiAccountStatus = aiaccountstatus.New(h.cfg, h.authManager, func(tenantID string) *managementapitools.Service {
			return h.APITools().serviceForTenant(tenantID)
		}, h.invalidateAIAccountCaches)
	}
	return h.aiAccountStatus
}

// StartAccountStatusScheduler starts (or restarts) the background quota probe.
//
// Called from server startup rather than from the first panel request: the
// whole point is to keep snapshots current for accounts nobody is looking at,
// which the lazily-built service can never do on its own.
func (h *Handler) StartAccountStatusScheduler() {
	if h == nil {
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg == nil || !cfg.AccountStatusRefresh.Enabled {
		h.stopAccountStatusScheduler()
		return
	}
	interval := time.Duration(cfg.AccountStatusRefresh.IntervalMinutes) * time.Minute
	if interval <= 0 {
		h.stopAccountStatusScheduler()
		return
	}
	scheduler := aiaccountstatus.NewScheduler(
		h.aiAccountStatusService,
		func() *coreauth.Manager {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.authManager
		},
		interval,
		time.Duration(cfg.AccountStatusRefresh.StartupDelaySeconds)*time.Second,
	)

	h.mu.Lock()
	previous := h.statusScheduler
	h.statusScheduler = scheduler
	h.mu.Unlock()
	if previous != nil {
		previous.Stop()
	}
	scheduler.Start()
}

func (h *Handler) stopAccountStatusScheduler() {
	if h == nil {
		return
	}
	h.mu.Lock()
	scheduler := h.statusScheduler
	h.statusScheduler = nil
	h.mu.Unlock()
	if scheduler != nil {
		scheduler.Stop()
	}
}

type aiAccountStatusRefreshBody struct {
	AuthIndexes    []string `json:"auth_indexes"`
	AuthIndexesAlt []string `json:"authIndexes"`
	AuthSubjectIDs []string `json:"auth_subject_ids"`
	AuthSubjects   []string `json:"authSubjectIds"`
	Force          bool     `json:"force"`
}

// PostAIAccountStatusRefresh starts a bounded async refresh job (202).
func (h *Handler) PostAIAccountStatusRefresh(c *gin.Context) {
	svc := h.aiAccountStatusService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "status service unavailable"})
		return
	}
	var body aiAccountStatusRefreshBody
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	indexes := append([]string{}, body.AuthIndexes...)
	indexes = append(indexes, body.AuthIndexesAlt...)
	subjects := append([]string{}, body.AuthSubjectIDs...)
	subjects = append(subjects, body.AuthSubjects...)

	accepted := svc.StartRefresh(effectiveTenantID(c), aiaccountstatus.RefreshRequest{
		AuthIndexes:    indexes,
		AuthSubjectIDs: subjects,
		Force:          body.Force,
	})
	c.JSON(http.StatusAccepted, accepted)
}

// GetAIAccountStatusRefreshJob returns job progress for the current tenant only.
func (h *Handler) GetAIAccountStatusRefreshJob(c *gin.Context) {
	svc := h.aiAccountStatusService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "status service unavailable"})
		return
	}
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "job_id is required"})
		return
	}
	snap, ok := svc.GetJob(effectiveTenantID(c), jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, snap)
}

// GetAIAccountStatus returns latest status + lightweight usage summary.
// Does not scan request_logs; reads current-tenant bindings and shared subject small tables.
func (h *Handler) GetAIAccountStatus(c *gin.Context) {
	svc := h.aiAccountStatusService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "status service unavailable"})
		return
	}
	indexes := queryStringListMulti(c, "auth_index", "auth_indexes")
	subjects := queryStringListMulti(c, "auth_subject_id", "auth_subject_ids")
	payload, err := svc.ListStatus(effectiveTenantID(c), indexes, subjects)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range payload.Items {
		if payload.Items[i].Quotas == nil {
			payload.Items[i].Quotas = []usage.QuotaWindowDTO{}
		}
	}
	c.JSON(http.StatusOK, payload)
}
