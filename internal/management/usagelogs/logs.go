package usagelogs

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (s *Service) ManagementLogs(input ManagementLogQueryInput) (map[string]any, error) {
	maps := s.buildNameMaps()
	params := s.managementLogParams(input, maps)

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
			if name, ok := maps.keyNameMap[item.APIKeyID]; ok {
				item.APIKeyName = name
			}
		}
		if channelName := displayChannelNameForLog(*item, maps.channelNameMap, maps.authIndexChannelMap, maps.ambiguousAuthIndexChannelMap); channelName != "" {
			item.ChannelName = channelName
		}
		enrichLogRowChannelMeta(item, maps.authMetaByIndex, maps.authMetaBySubject)
	}

	if filters.APIKeyIDNames == nil {
		filters.APIKeyIDNames = make(map[string]string, len(filters.APIKeyIDs))
	}
	// Prefer the account display names already resolved by the key facet. Only
	// fill gaps from keyNameMap; never overwrite end-user labels with key names.
	for _, id := range filters.APIKeyIDs {
		if strings.TrimSpace(filters.APIKeyIDNames[id]) != "" {
			continue
		}
		if name, ok := maps.keyNameMap[id]; ok {
			filters.APIKeyIDNames[id] = name
		}
	}
	filters.ChannelOptions = enrichChannelFilterOptions(
		filters.ChannelOptions,
		maps.channelNameMap,
		maps.authIndexChannelMap,
		maps.authMetaByIndex,
		maps.authIndexGroup,
		maps.authSubjectByIndex,
		maps.authMetaBySubject,
	)
	filters.Channels = channelLabelsFromOptions(filters.ChannelOptions)

	if result.Items == nil {
		result.Items = make([]usage.LogRow, 0)
	}
	if filters.APIKeyIDs == nil {
		filters.APIKeyIDs = make([]string, 0)
	}
	if filters.APIKeyIDCounts == nil {
		filters.APIKeyIDCounts = make(map[string]int64)
	}
	if filters.Models == nil {
		filters.Models = make([]string, 0)
	}
	if filters.Channels == nil {
		filters.Channels = make([]string, 0)
	}
	if filters.ChannelOptions == nil {
		filters.ChannelOptions = make([]usage.ChannelFilterOption, 0)
	}
	if filters.Statuses == nil {
		filters.Statuses = make([]string, 0)
	}

	return map[string]any{
		"items":   result.Items,
		"total":   result.Total,
		"page":    result.Page,
		"size":    result.Size,
		"filters": filters,
		"stats":   stats,
		// The panel reads this to tell a backend that has dropped the secret-keyed
		// filter facets (api_keys, api_key_names, api_key_counts) from an older one.
		"capabilities": map[string]bool{CapabilityAPIKeyIDFilters: true},
	}, nil
}

// CapabilityAPIKeyIDFilters is declared by /usage/logs once the key filter is
// id-based only: filters.api_key_ids / api_key_id_names / api_key_id_counts
// describe the keys (with "__system__" for system calls), the api_key_ids and
// api_key_ids_empty query parameters select them, every row's api_key is a mask,
// and the secret-keyed facets no longer exist.
const CapabilityAPIKeyIDFilters = "api-key-id-filters"

func (s *Service) ClearAllRequestLogs() (any, error) {
	return usage.ClearAllRequestLogsForTenant(s.tenantID)
}

func (s *Service) ClearRequestLogs(options usage.ClearRequestLogsOptions) (int, any, error) {
	options.TenantID = s.tenantID
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
	maps := s.buildNameMaps()
	authSubjectIDs, authIndexes, channelNames, authIndexChannelNames := channelFilterSelectors(
		input.Channels,
		maps.channelNameMap,
		maps.authIndexChannelMap,
		maps.ambiguousAuthIndexChannelMap,
		maps.authMetaByIndex,
		maps.authIndexGroup,
		maps.authSubjectByIndex,
		maps.authIndexesBySubject,
		maps.authMetaBySubject,
	)
	params := usage.LogQueryParams{
		TenantID:              s.tenantID,
		EndUserID:             strings.TrimSpace(input.EndUserID),
		Page:                  input.Page,
		Size:                  input.Size,
		Days:                  input.Days,
		Models:                input.Models,
		Statuses:              input.Statuses,
		MatchNoModels:         input.MatchNoModels,
		MatchNoChannels:       input.MatchNoChannels,
		MatchNoStatuses:       input.MatchNoStatuses,
		AuthSubjectIDs:        authSubjectIDs,
		AuthIndexes:           authIndexes,
		ChannelNames:          channelNames,
		AuthIndexChannelNames: authIndexChannelNames,
	}
	if params.EndUserID == "" {
		params.TenantID = usage.ResolveAPIKeyTenant(input.APIKey)
		params.APIKeys = usage.ExpandPublicLookupAPIKeys(input.APIKey)
	}
	// Scope key-id filters to the authenticated subject only (prevents IDOR).
	allowedKeyIDs := publicAllowedAPIKeyIDs(params.TenantID, params.EndUserID, params.APIKeys)
	params.APIKeyIDs, params.MatchNoAPIKeyIDs = constrainPublicAPIKeyIDs(
		input.APIKeyIDs,
		input.MatchNoAPIKeyIDs,
		allowedKeyIDs,
	)

	result, err := usage.QueryLogs(params)
	if err != nil {
		return nil, err
	}
	stats, err := usage.QueryStats(params)
	if err != nil {
		return nil, err
	}
	filters, err := usage.QueryFiltersForLogs(params)
	if err != nil {
		return nil, err
	}
	// Restrict public key facet options to keys owned by the lookup subject.
	filters.APIKeyIDs, filters.APIKeyIDNames, filters.APIKeyIDCounts = filterPublicAPIKeyIDOptions(
		allowedKeyIDs,
		filters.APIKeyIDs,
		filters.APIKeyIDNames,
		filters.APIKeyIDCounts,
	)

	// Top-level api_key_name:
	// - raw secret lookup (/apikey-usage): always the *presented key's own name*
	// - portal session (no raw key, EndUserID only): account display name
	// Never label a secret lookup with end-user display name (e.g. 张军宝).
	presentedKey := strings.TrimSpace(input.APIKey)
	apiKeyName := ""
	if presentedKey != "" {
		apiKeyName = usage.ResolveAPIKeyOwnName(presentedKey)
		if apiKeyName == "" {
			apiKeyName = s.publicAPIKeyName(presentedKey)
		}
	} else if params.EndUserID != "" {
		apiKeyName = usage.DisplayNameForEndUser(params.EndUserID)
	}
	for i := range result.Items {
		keyOwnName := strings.TrimSpace(result.Items[i].APIKeyOwnName)
		if keyOwnName == "" {
			keyOwnName = strings.TrimSpace(result.Items[i].APIKeyName)
		}
		userName := strings.TrimSpace(result.Items[i].EndUserDisplayName)
		if userName == "" && params.EndUserID != "" {
			userName = usage.DisplayNameForEndUser(params.EndUserID)
		}
		if userName == "" {
			userName = keyOwnName
		}
		// Do not overwrite top-level apiKeyName with per-row user/key labels.
		channelName := displayChannelNameForLog(result.Items[i], maps.channelNameMap, maps.authIndexChannelMap, maps.ambiguousAuthIndexChannelMap)
		// APIKeyMasked was set with the row's display key; the secret itself is not
		// part of a public row.
		result.Items[i].Source = ""
		result.Items[i].AuthIndex = ""
		result.Items[i].AuthSubjectID = ""
		result.Items[i].ChannelName = channelName
		result.Items[i].APIKey = ""
		result.Items[i].APIKeyID = ""
		// The client session id is kept for the console; the public lookup has
		// no use for it and does not hand it out.
		result.Items[i].SessionID = ""
		result.Items[i].APIKeyName = userName
		result.Items[i].EndUserDisplayName = userName
		result.Items[i].APIKeyOwnName = keyOwnName
		// Keep provider/auth_type for public UI badges, but strip identity keys above.
		enrichLogRowChannelMeta(&result.Items[i], maps.authMetaByIndex, maps.authMetaBySubject)
		result.Items[i].AuthIndex = ""
		result.Items[i].AuthSubjectID = ""
	}

	filters.ChannelOptions = enrichChannelFilterOptions(
		filters.ChannelOptions,
		maps.channelNameMap,
		maps.authIndexChannelMap,
		maps.authMetaByIndex,
		maps.authIndexGroup,
		maps.authSubjectByIndex,
		maps.authMetaBySubject,
	)
	// Public responses keep opaque filter values so same-email multi-provider
	// accounts stay selectable, but strip explicit identity fields.
	for i := range filters.ChannelOptions {
		filters.ChannelOptions[i].AuthIndex = ""
		filters.ChannelOptions[i].AuthSubjectID = ""
	}
	filters.Channels = channelLabelsFromOptions(filters.ChannelOptions)
	if result.Items == nil {
		result.Items = make([]usage.LogRow, 0)
	}
	if filters.Models == nil {
		filters.Models = make([]string, 0)
	}
	if filters.Channels == nil {
		filters.Channels = make([]string, 0)
	}
	if filters.ChannelOptions == nil {
		filters.ChannelOptions = make([]usage.ChannelFilterOption, 0)
	}
	if filters.Statuses == nil {
		filters.Statuses = make([]string, 0)
	}
	if filters.APIKeyIDs == nil {
		filters.APIKeyIDs = make([]string, 0)
	}
	if filters.APIKeyIDNames == nil {
		filters.APIKeyIDNames = make(map[string]string)
	}
	if filters.APIKeyIDCounts == nil {
		filters.APIKeyIDCounts = make(map[string]int64)
	}

	return map[string]any{
		"items":        result.Items,
		"total":        result.Total,
		"page":         result.Page,
		"size":         result.Size,
		"stats":        stats,
		"api_key_name": apiKeyName,
		"filters": map[string]any{
			"api_key_ids":       filters.APIKeyIDs,
			"api_key_id_names":  filters.APIKeyIDNames,
			"api_key_id_counts": filters.APIKeyIDCounts,
			"models":            filters.Models,
			"channels":          filters.Channels,
			"channel_options":   filters.ChannelOptions,
			"statuses":          filters.Statuses,
		},
	}, nil
}
