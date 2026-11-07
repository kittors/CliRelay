package usage

import (
	"fmt"

	log "github.com/sirupsen/logrus"
)

// QueryStats returns aggregated statistics for the same filters as QueryLogs.
// It prefers usage rollups, falling back to request_logs only when the rollup
// dimensions cannot express the requested filter without changing its meaning.
func QueryStats(params LogQueryParams) (LogStats, error) {
	if getReadDB() == nil {
		return LogStats{CacheRate: 0}, nil
	}
	params = normalizeLogQueryParams(params)
	// Explicit empty multi-selects match no rows.
	if params.MatchNoAPIKeys || params.MatchNoAPIKeyIDs || params.MatchNoModels || params.MatchNoStatuses || params.MatchNoChannels {
		return LogStats{CacheRate: 0}, nil
	}

	filter, ok := rollupIdentityFilter(params)
	if !ok {
		// Preserve fail-closed identity behavior before considering a detail fallback.
		return LogStats{CacheRate: 0}, nil
	}

	var (
		stats LogStats
		err   error
	)
	if queryStatsRequiresDetailAggregation(params) {
		stats, err = queryStatsFromDetails(params)
	} else {
		stats, err = queryStatsFromRollupFilter(params, filter)
	}
	if err != nil {
		log.Warnf("usage: stats query failed: %v", err)
		return LogStats{}, err
	}
	return stats, nil
}

func queryStatsRequiresDetailAggregation(params LogQueryParams) bool {
	// auth_index is not a rollup dimension, and precise legacy channel pairs
	// cannot be represented by the rollup dimensions.
	if len(params.AuthIndexes) > 0 || len(params.AuthIndexChannelNames) > 0 {
		return true
	}

	// Which check ended a request is a column of the detail rows only.
	if condition, _ := guardKindCondition(params.GuardKinds); condition != "" {
		return true
	}

	// QueryLogs combines channel selector kinds with OR, while rollup dimensions
	// are conjunctive. A mixed selector therefore needs the shared detail WHERE.
	channelSelectorKinds := 0
	if len(dedupeExactStrings(params.AuthSubjectIDs)) > 0 {
		channelSelectorKinds++
	}
	if len(dedupeLowerTrimmedStrings(params.ChannelNames)) > 0 {
		channelSelectorKinds++
	}
	if channelSelectorKinds > 1 {
		return true
	}

	// Rollup buckets keep status counts, but token/cost/cache totals are not split
	// by status. Use details for an exact single-status aggregate.
	hasSuccess, hasFailed := requestedStatusFilter(params.Statuses)
	return hasSuccess != hasFailed
}

func queryStatsFromDetails(params LogQueryParams) (LogStats, error) {
	db := getReadDB()
	if db == nil {
		return LogStats{CacheRate: 0}, nil
	}
	where, args := buildWhereClause(params)

	var agg rollupAgg
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost), 0),
			COALESCE(SUM(cached_tokens), 0),` + requestLogCacheSumsSQL + `
		FROM request_logs` + where
	if err := db.QueryRow(query, args...).Scan(append([]any{
		&agg.RequestCount,
		&agg.SuccessCount,
		&agg.TotalTokens,
		&agg.CostTotal,
		&agg.CachedTokens,
	}, agg.Cache.scanTargets()...)...); err != nil {
		return LogStats{}, fmt.Errorf("usage: detail stats aggregate: %w", err)
	}
	return agg.toLogStats(), nil
}
