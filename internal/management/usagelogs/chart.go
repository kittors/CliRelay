package usagelogs

import "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"

func (s *Service) PublicChartData(apiKey string, days int) (map[string]any, error) {
	chartData, err := usage.QueryPublicChartData(apiKey, days)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"daily_series":         chartData.DailySeries,
		"heatmap_series":       chartData.HeatmapSeries,
		"model_distribution":   chartData.ModelDistribution,
		"api_key_distribution": chartData.APIKeyDistribution,
		"stats":                chartData.Stats,
		// Key own name only — never end-user display name.
		"api_key_name": s.publicAPIKeyName(apiKey),
	}, nil
}

func (s *Service) PublicChartDataForEndUser(endUserID string, days int) (map[string]any, error) {
	chartData, err := usage.QueryPublicChartDataForEndUser(s.tenantID, endUserID, days)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"daily_series":         chartData.DailySeries,
		"heatmap_series":       chartData.HeatmapSeries,
		"model_distribution":   chartData.ModelDistribution,
		"api_key_distribution": chartData.APIKeyDistribution,
		"stats":                chartData.Stats,
		"api_key_name":         usage.DisplayNameForEndUser(endUserID),
	}, nil
}

func (s *Service) UsageChartData(apiKey string, days int) (map[string]any, error) {
	daily, err := usage.QueryDailySeriesForTenant(s.tenantID, apiKey, days)
	if err != nil {
		return nil, err
	}
	if daily == nil {
		daily = []usage.DailySeriesPoint{}
	}

	models, err := usage.QueryModelDistributionForTenant(s.tenantID, apiKey, days)
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []usage.ModelDistributionPoint{}
	}

	hourlyTokens, hourlyModels, err := usage.QueryHourlySeriesForTenant(s.tenantID, apiKey, 24)
	if err != nil {
		return nil, err
	}
	if hourlyTokens == nil {
		hourlyTokens = []usage.HourlyTokenPoint{}
	}
	if hourlyModels == nil {
		hourlyModels = []usage.HourlyModelPoint{}
	}

	var apikeyDist []usage.APIKeyDistributionPoint
	if apiKey == "" {
		apikeyDist, err = usage.QueryAPIKeyDistributionForTenant(s.tenantID, days)
		if err != nil {
			return nil, err
		}
		maps := s.buildNameMaps()

		for i := range apikeyDist {
			if apikeyDist[i].Name == "" {
				if name, ok := maps.keyNameMap[apikeyDist[i].APIKey]; ok {
					apikeyDist[i].Name = name
				}
			}
		}
	}
	if apikeyDist == nil {
		apikeyDist = []usage.APIKeyDistributionPoint{}
	}

	return map[string]any{
		"daily_series":        daily,
		"model_distribution":  models,
		"hourly_tokens":       hourlyTokens,
		"hourly_models":       hourlyModels,
		"apikey_distribution": apikeyDist,
	}, nil
}

func (s *Service) EntityUsageStats(apiKey string, days int, authIndexes, sources []string) (map[string]any, error) {
	sourceStats, err := usage.QueryEntityStatsForTenant(s.tenantID, apiKey, days, "source", sources)
	if err != nil {
		return nil, err
	}
	if sourceStats == nil {
		sourceStats = []usage.EntityStatPoint{}
	}

	authIndexStats, err := usage.QueryEntityStatsForTenant(s.tenantID, apiKey, days, "auth_index", authIndexes)
	if err != nil {
		return nil, err
	}
	if authIndexStats == nil {
		authIndexStats = []usage.EntityStatPoint{}
	}

	return map[string]any{
		"source":     sourceStats,
		"auth_index": authIndexStats,
	}, nil
}
