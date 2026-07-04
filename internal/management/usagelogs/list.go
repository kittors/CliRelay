package usagelogs

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (s *Service) ManagementLogs(input ManagementLogQueryInput) (map[string]any, error) {
	keyNameMap, channelNameMap, authIndexChannelMap, ambiguousAuthIndexChannelMap := s.buildNameMaps()

	selectedChannelKeys := make(map[string]struct{})
	for _, part := range input.Channels {
		key := strings.ToLower(strings.TrimSpace(part))
		if key == "" {
			continue
		}
		selectedChannelKeys[key] = struct{}{}
	}

	var authIndexes []string
	var channelNames []string
	authIndexChannelNames := make(map[string][]string)
	if len(selectedChannelKeys) > 0 {
		for key := range selectedChannelKeys {
			channelNames = append(channelNames, key)
		}
		for raw, name := range channelNameMap {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if _, ok := selectedChannelKeys[key]; ok {
				channelNames = append(channelNames, raw)
			}
		}
		for idx, name := range authIndexChannelMap {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if _, ok := selectedChannelKeys[key]; ok {
				authIndexes = append(authIndexes, idx)
				if legacyChannels := ambiguousAuthIndexChannelMap[idx]; len(legacyChannels) > 0 {
					authIndexChannelNames[idx] = append(authIndexChannelNames[idx], legacyChannels...)
				}
			}
		}
		if len(authIndexes) == 0 && len(channelNames) == 0 {
			authIndexes = []string{""}
		}
	}

	params := usage.LogQueryParams{
		Page:                  input.Page,
		Size:                  input.Size,
		Days:                  input.Days,
		APIKeys:               input.APIKeys,
		Models:                input.Models,
		Statuses:              input.Statuses,
		MatchNoAPIKeys:        input.MatchNoAPIKeys,
		MatchNoModels:         input.MatchNoModels,
		MatchNoStatuses:       input.MatchNoStatuses,
		MatchNoChannels:       input.MatchNoChannels,
		AuthIndexes:           authIndexes,
		ChannelNames:          channelNames,
		AuthIndexChannelNames: authIndexChannelNames,
	}

	result, err := usage.QueryLogs(params)
	if err != nil {
		return nil, err
	}
	filters, err := usage.QueryFiltersForLogs(params)
	if err != nil {
		return nil, err
	}
	stats, err := usage.QueryStats(params)
	if err != nil {
		return nil, err
	}

	for i := range result.Items {
		item := &result.Items[i]
		if item.APIKeyName == "" {
			if name, ok := keyNameMap[item.APIKey]; ok {
				item.APIKeyName = name
			}
		}
		if item.ChannelName != "" {
			if name, ok := authIndexChannelMap[item.AuthIndex]; ok && strings.TrimSpace(name) != "" {
				if _, legacy := channelNameMap[item.ChannelName]; legacy || containsFold(ambiguousAuthIndexChannelMap[item.AuthIndex], item.ChannelName) {
					item.ChannelName = name
					continue
				}
			}
			if name, ok := channelNameMap[item.ChannelName]; ok && strings.TrimSpace(name) != "" {
				item.ChannelName = name
			}
			continue
		}
		if name, ok := authIndexChannelMap[item.AuthIndex]; ok && strings.TrimSpace(name) != "" {
			item.ChannelName = name
			continue
		}
		if name, ok := channelNameMap[item.Source]; ok {
			item.ChannelName = name
		}
	}

	if filters.APIKeyNames == nil {
		filters.APIKeyNames = make(map[string]string, len(filters.APIKeys))
	}
	for _, key := range filters.APIKeys {
		if name, ok := keyNameMap[key]; ok {
			filters.APIKeyNames[key] = name
		}
	}
	if len(filters.Channels) > 0 {
		seen := make(map[string]struct{})
		channels := make([]string, 0, len(filters.Channels))
		for _, value := range filters.Channels {
			trimmed := strings.TrimSpace(value)
			if name, ok := channelNameMap[trimmed]; ok && strings.TrimSpace(name) != "" {
				trimmed = strings.TrimSpace(name)
			}
			key := strings.ToLower(trimmed)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			channels = append(channels, trimmed)
		}
		sort.Slice(channels, func(i, j int) bool { return strings.ToLower(channels[i]) < strings.ToLower(channels[j]) })
		filters.Channels = channels
	}

	if result.Items == nil {
		result.Items = make([]usage.LogRow, 0)
	}
	if filters.APIKeys == nil {
		filters.APIKeys = make([]string, 0)
	}
	if filters.Models == nil {
		filters.Models = make([]string, 0)
	}
	if filters.Channels == nil {
		filters.Channels = make([]string, 0)
	}
	if filters.Statuses == nil {
		filters.Statuses = make([]string, 0)
	}
	if filters.APIKeyNames == nil {
		filters.APIKeyNames = make(map[string]string)
	}

	return map[string]any{
		"items":   result.Items,
		"total":   result.Total,
		"page":    result.Page,
		"size":    result.Size,
		"filters": filters,
		"stats":   stats,
	}, nil
}

func (s *Service) ClearAllRequestLogs() (any, error) {
	return usage.ClearAllRequestLogs()
}

func (s *Service) ClearRequestLogs(options usage.ClearRequestLogsOptions) (int, any, error) {
	result, err := usage.ClearRequestLogs(options)
	if err != nil {
		if strings.Contains(err.Error(), "at least one cleanup option") {
			return http.StatusBadRequest, map[string]any{"error": err.Error()}, err
		}
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}, err
	}
	return http.StatusOK, result, nil
}

func (s *Service) PublicUsageLogs(input PublicLogQueryInput) (map[string]any, error) {
	params := usage.LogQueryParams{
		Page:   input.Page,
		Size:   input.Size,
		Days:   input.Days,
		APIKey: input.APIKey,
		Model:  input.Model,
		Status: input.Status,
	}

	result, err := usage.QueryLogs(params)
	if err != nil {
		return nil, err
	}
	stats, err := usage.QueryStats(params)
	if err != nil {
		return nil, err
	}

	for i := range result.Items {
		result.Items[i].Source = ""
		result.Items[i].AuthIndex = ""
		result.Items[i].ChannelName = ""
		result.Items[i].APIKey = ""
		result.Items[i].APIKeyName = ""
	}

	models, _ := usage.QueryModelsForKey(input.APIKey, params.Days)
	if models == nil {
		models = make([]string, 0)
	}

	return map[string]any{
		"items": result.Items,
		"total": result.Total,
		"page":  result.Page,
		"size":  result.Size,
		"stats": stats,
		"filters": map[string]any{
			"models": models,
		},
	}, nil
}

func (s *Service) buildNameMaps() (keyNameMap, channelNameMap, authIndexChannelMap map[string]string, ambiguousAuthIndexChannelMap map[string][]string) {
	keyNameMap = make(map[string]string)
	channelNameMap = make(map[string]string)
	authIndexChannelMap = make(map[string]string)
	ambiguousAuthIndexChannelMap = make(map[string][]string)

	for _, row := range apikeysettings.NewService(nil).ListRows() {
		if row.Key != "" && row.Name != "" {
			keyNameMap[row.Key] = row.Name
		}
	}

	cfg := s.cfg
	if cfg != nil {
		for _, k := range cfg.GeminiKey {
			if k.APIKey != "" && k.Name != "" {
				channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, k := range cfg.ClaudeKey {
			if k.APIKey != "" && k.Name != "" {
				channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, k := range cfg.CodexKey {
			if k.APIKey != "" && k.Name != "" {
				channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, provider := range cfg.OpenAICompatibility {
			if provider.Name == "" {
				continue
			}
			for _, entry := range provider.APIKeyEntries {
				if entry.APIKey != "" {
					channelNameMap[entry.APIKey] = provider.Name
				}
			}
		}
	}

	type legacyChannelCandidate struct {
		key       string
		channel   string
		authIndex string
	}
	var legacyCandidates []legacyChannelCandidate

	if s.authManager != nil {
		for _, auth := range s.authManager.List() {
			if auth == nil {
				continue
			}
			channel := strings.TrimSpace(auth.ChannelName())
			if channel == "" {
				continue
			}
			auth.EnsureIndex()
			if idx := strings.TrimSpace(auth.Index); idx != "" {
				authIndexChannelMap[idx] = channel
			}
			if accountType, account := auth.AccountInfo(); strings.EqualFold(accountType, "oauth") {
				if source := strings.TrimSpace(account); source != "" {
					legacyCandidates = append(legacyCandidates, legacyChannelCandidate{key: source, channel: channel, authIndex: strings.TrimSpace(auth.Index)})
				}
			}
			if email := strings.TrimSpace(managementauthfiles.Email(auth)); email != "" {
				legacyCandidates = append(legacyCandidates, legacyChannelCandidate{key: email, channel: channel, authIndex: strings.TrimSpace(auth.Index)})
			}
		}
	}

	legacyChannelsByKey := make(map[string]map[string]struct{})
	for _, candidate := range legacyCandidates {
		key := strings.TrimSpace(candidate.key)
		channel := strings.TrimSpace(candidate.channel)
		if key == "" || channel == "" {
			continue
		}
		if legacyChannelsByKey[key] == nil {
			legacyChannelsByKey[key] = make(map[string]struct{})
		}
		legacyChannelsByKey[key][strings.ToLower(channel)] = struct{}{}
	}
	for _, candidate := range legacyCandidates {
		key := strings.TrimSpace(candidate.key)
		if key == "" {
			continue
		}
		if len(legacyChannelsByKey[key]) > 1 {
			if candidate.authIndex != "" {
				ambiguousAuthIndexChannelMap[candidate.authIndex] = append(ambiguousAuthIndexChannelMap[candidate.authIndex], key)
			}
			continue
		}
		channelNameMap[key] = strings.TrimSpace(candidate.channel)
	}

	return
}

func containsFold(values []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), needle) {
			return true
		}
	}
	return false
}

func IntQueryDefault(raw string, def int) int {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
