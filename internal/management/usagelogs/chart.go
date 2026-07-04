package usagelogs

import "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"

func (s *Service) PublicChartData(apiKey string, days int) (map[string]any, error) {
	daily, err := usage.QueryDailySeries(apiKey, days)
	if err != nil {
		return nil, err
	}
	if daily == nil {
		daily = []usage.DailySeriesPoint{}
	}

	models, err := usage.QueryModelDistribution(apiKey, days)
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []usage.ModelDistributionPoint{}
	}

	stats, err := usage.QueryStats(usage.LogQueryParams{APIKey: apiKey, Days: days})
	if err != nil {
		return nil, err
	}
	stats.TotalSessions, err = usage.QuerySessionCount(usage.LogQueryParams{APIKey: apiKey, Days: days})
	if err != nil {
		return nil, err
	}

	heatmap, err := usage.QueryDailyHeatmapSeries(apiKey, 365)
	if err != nil {
		return nil, err
	}
	if heatmap == nil {
		heatmap = []usage.DailyHeatmapPoint{}
	}

	return map[string]any{
		"daily_series":       daily,
		"heatmap_series":     heatmap,
		"model_distribution": models,
		"stats":              stats,
		"api_key_name":       s.publicAPIKeyName(apiKey),
	}, nil
}

func (s *Service) UsageChartData(apiKey string, days int) (map[string]any, error) {
	daily, err := usage.QueryDailySeries(apiKey, days)
	if err != nil {
		return nil, err
	}
	if daily == nil {
		daily = []usage.DailySeriesPoint{}
	}

	models, err := usage.QueryModelDistribution(apiKey, days)
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []usage.ModelDistributionPoint{}
	}

	hourlyTokens, hourlyModels, err := usage.QueryHourlySeries(apiKey, 24)
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
		apikeyDist, err = usage.QueryAPIKeyDistribution(days)
		if err != nil {
			return nil, err
		}
		keyNameMap, _, _, _ := s.buildNameMaps()
		for i := range apikeyDist {
			if apikeyDist[i].Name == "" {
				if name, ok := keyNameMap[apikeyDist[i].APIKey]; ok {
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
	sourceStats, err := usage.QueryEntityStats(apiKey, days, "source", sources)
	if err != nil {
		return nil, err
	}
	if sourceStats == nil {
		sourceStats = []usage.EntityStatPoint{}
	}

	authIndexStats, err := usage.QueryEntityStats(apiKey, days, "auth_index", authIndexes)
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
