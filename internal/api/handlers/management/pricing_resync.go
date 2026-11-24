package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/modelcatalog"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// A price edit reaches the requests already logged through a pricing resync job
// (see internal/usage/pricing_resync.go). By default the job only fills the
// requests stored without a cost; "resync_history": "reprice_7d" in the edit
// asks it to recompute every request of the last seven days with the new price,
// to correct a wrong one.

const resyncHistoryReprice = "reprice_7d"

// modelConfigEditRequest is a model config payload plus how the edit treats
// the requests already logged.
type modelConfigEditRequest struct {
	modelcatalog.ModelConfigPayload
	ResyncHistory string `json:"resync_history"`
}

func resyncModeFor(value string) string {
	if strings.TrimSpace(value) == resyncHistoryReprice {
		return usage.PricingResyncReprice
	}
	return usage.PricingResyncFill
}

// pricingFingerprints records the price each model is billed with in a tenant
// before an edit.
func pricingFingerprints(tenantID string, modelIDs ...string) map[string]string {
	out := make(map[string]string, len(modelIDs))
	for _, id := range modelIDs {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = usage.EffectivePricingFingerprint(tenantID, id)
		}
	}
	return out
}

// queuePricingResyncAfterEdit queues a job for the models whose price the edit
// changed. A reprice request with an unchanged price still runs: correcting
// history is what it asks for. A failure to queue does not fail the edit; the
// worker's reconcile pass fills missing costs on its own.
func queuePricingResyncAfterEdit(tenantID string, before map[string]string, mode string) {
	changed := make([]string, 0, len(before))
	for id, fingerprint := range before {
		if mode == usage.PricingResyncReprice || usage.EffectivePricingFingerprint(tenantID, id) != fingerprint {
			changed = append(changed, id)
		}
	}
	if len(changed) == 0 {
		return
	}
	if _, err := usage.EnqueuePricingResync(tenantID, changed, mode, usage.PricingResyncOriginManual); err != nil {
		log.Warnf("management: queue pricing resync for %v: %v", changed, err)
	}
}

// GetPricingResyncJobs returns the tenant's latest pricing resync jobs.
func (h *ModelsHandler) GetPricingResyncJobs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	jobs, err := usage.ListPricingResyncJobs(effectiveTenantID(c), limit)
	if err != nil {
		writeInternalError(c, "查询模型定价重算任务列表", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}
