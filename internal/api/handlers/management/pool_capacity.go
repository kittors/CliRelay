package management

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// GET /ai-accounts/pool-capacity answers "how much is left across all the
// accounts of a provider, and how long will it last". It adds up what the status
// probes already stored (see alerting.BuildPools), so it costs the upstream
// nothing; what it does cost is one pass over the tenant's accounts and one
// indexed query for the last six hours of readings, which is why the answer is
// kept for a short while and shared between concurrent callers instead of being
// recomputed for every panel that is open.

const (
	// poolCapacityTTL is how long a computed answer is reused. Probes arrive
	// minutes apart, so a fresher answer would only differ by the noise of the
	// moment, and a person watching the card gains nothing from sub-minute refresh.
	poolCapacityTTL = 30 * time.Second

	// poolCapacityBudget bounds one computation. It runs detached from the request
	// that triggered it (other callers are waiting on the same result), so a client
	// that went away neither wastes nor poisons it.
	poolCapacityBudget = 20 * time.Second

	// poolCapacityMaxTenants keeps the cache from growing with the number of
	// tenants that ever asked; expired entries are dropped past this size.
	poolCapacityMaxTenants = 256
)

// poolCapacityResponse is the body of GET /ai-accounts/pool-capacity. The panel
// declares the contract (PoolCapacityResponse in @code-proxy/api-client); the
// optional fields are left out, not zeroed, when there is nothing to say, because
// the panel reads "absent" as "not estimated" and 0 as a measurement.
type poolCapacityResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Pools       []poolCapacityPool `json:"pools"`
}

type poolCapacityPool struct {
	Provider         string  `json:"provider"`
	WindowKey        string  `json:"window_key"`
	WindowLabel      string  `json:"window_label"`
	WindowSeconds    int64   `json:"window_seconds"`
	Accounts         int     `json:"accounts"`
	AccountsWithData int     `json:"accounts_with_data"`
	RemainingPercent float64 `json:"remaining_percent"`

	EarliestResetAt       *time.Time `json:"earliest_reset_at,omitempty"`
	BurnPercentPerHour    *float64   `json:"burn_percent_per_hour,omitempty"`
	ProjectedExhaustionAt *time.Time `json:"projected_exhaustion_at,omitempty"`
	ObservedAt            *time.Time `json:"observed_at,omitempty"`
}

// poolCapacityCache holds one answer per tenant.
type poolCapacityCache struct {
	mu      sync.Mutex
	entries map[string]poolCapacityCached
	flight  singleflight.Group
}

type poolCapacityCached struct {
	at       time.Time
	response poolCapacityResponse
}

// GetPoolCapacity returns the capacity of the effective tenant's account pools.
func (h *Handler) GetPoolCapacity(c *gin.Context) {
	h.servePoolCapacity(c, func(ctx context.Context, tenantID string, now time.Time) (poolCapacityResponse, error) {
		return computePoolCapacity(ctx, alertSources{authManager: h.currentAuthManager}, readPoolReadings, tenantID, now)
	})
}

// servePoolCapacity answers from the cache or from compute, and turns a failure
// into a body that says nothing about its cause: the log has that.
func (h *Handler) servePoolCapacity(c *gin.Context, compute func(ctx context.Context, tenantID string, now time.Time) (poolCapacityResponse, error)) {
	tenantID := effectiveTenantID(c)
	response, err := h.poolCache.get(c.Request.Context(), tenantID, time.Now(), func(ctx context.Context, now time.Time) (poolCapacityResponse, error) {
		return compute(ctx, tenantID, now)
	})
	if err != nil {
		log.WithError(err).Warn("pool capacity: could not be computed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "pool_capacity_failed", "message": "the pool capacity could not be computed"}})
		return
	}
	c.JSON(http.StatusOK, response)
}

// get returns the cached answer for the tenant if it is fresh, else computes
// one. Callers that arrive while a computation runs wait for it rather than
// starting their own.
func (c *poolCapacityCache) get(ctx context.Context, tenantID string, now time.Time, compute func(context.Context, time.Time) (poolCapacityResponse, error)) (poolCapacityResponse, error) {
	c.mu.Lock()
	if cached, ok := c.entries[tenantID]; ok && now.Sub(cached.at) < poolCapacityTTL && !now.Before(cached.at) {
		c.mu.Unlock()
		return cached.response, nil
	}
	c.mu.Unlock()

	resultCh := c.flight.DoChan(tenantID, func() (result any, err error) {
		// singleflight runs this on a goroutine of its own and re-raises a panic
		// there, where nothing (not the router's recovery) can catch it and the
		// whole process would go down. A bug in the computation must cost one
		// failed request instead.
		defer func() {
			if recovered := recover(); recovered != nil {
				result, err = nil, fmt.Errorf("pool capacity: panic while computing: %v", recovered)
			}
		}()
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolCapacityBudget)
		defer cancel()
		response, err := compute(runCtx, now)
		if err != nil {
			return nil, err
		}
		c.store(tenantID, now, response)
		return response, nil
	})
	select {
	case result := <-resultCh:
		if result.Err != nil {
			return poolCapacityResponse{}, result.Err
		}
		return result.Val.(poolCapacityResponse), nil
	case <-ctx.Done():
		return poolCapacityResponse{}, ctx.Err()
	}
}

func (c *poolCapacityCache) store(tenantID string, now time.Time, response poolCapacityResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]poolCapacityCached)
	}
	if len(c.entries) >= poolCapacityMaxTenants {
		for id, cached := range c.entries {
			if now.Sub(cached.at) >= poolCapacityTTL {
				delete(c.entries, id)
			}
		}
	}
	c.entries[tenantID] = poolCapacityCached{at: now, response: response}
}

// poolReadingsFunc loads the stored readings of the named windows since a time.
type poolReadingsFunc func(subjects, windows []string, since time.Time) ([]usage.AIAccountQuotaReading, error)

func readPoolReadings(subjects, windows []string, since time.Time) ([]usage.AIAccountQuotaReading, error) {
	return usage.QueryAIAccountSubjectQuotaReadings(subjects, windows, since)
}

// computePoolCapacity builds the answer from the tenant's accounts and the
// readings stored for them.
func computePoolCapacity(ctx context.Context, sources alerting.Sources, readings poolReadingsFunc, tenantID string, now time.Time) (poolCapacityResponse, error) {
	accounts, err := sources.Accounts(ctx, tenantID, now)
	if err != nil {
		return poolCapacityResponse{}, err
	}
	history := alerting.PoolHistory{}
	if subjects, windows := alerting.PoolHistoryScope(accounts); len(subjects) > 0 {
		stored, err := readings(subjects, windows, now.Add(-alerting.PoolLookback))
		if err != nil {
			return poolCapacityResponse{}, err
		}
		for _, r := range stored {
			// A reading with no percentage says nothing about how much is left.
			if r.RemainingPercent == nil {
				continue
			}
			series := alerting.PoolSeries{Subject: r.AuthSubjectID, Window: r.QuotaKey}
			history[series] = append(history[series], alerting.PoolReading{
				At: r.RecordedAt, RemainingPercent: *r.RemainingPercent, ResetAt: r.ResetAt,
			})
		}
	}
	pools := alerting.BuildPools(accounts, history, now)
	response := poolCapacityResponse{GeneratedAt: now.UTC(), Pools: make([]poolCapacityPool, 0, len(pools))}
	for _, p := range pools {
		response.Pools = append(response.Pools, poolCapacityPool{
			Provider: p.Provider, WindowKey: p.WindowKey, WindowLabel: p.WindowLabel, WindowSeconds: p.WindowSeconds,
			Accounts: p.Accounts, AccountsWithData: p.AccountsWithData, RemainingPercent: p.RemainingPercent,
			EarliestResetAt: timeOrNil(p.EarliestResetAt), BurnPercentPerHour: p.BurnPercentPerHour,
			ProjectedExhaustionAt: timeOrNil(p.ProjectedExhaustionAt), ObservedAt: timeOrNil(p.ObservedAt),
		})
	}
	return response, nil
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
