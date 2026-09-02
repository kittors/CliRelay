package apikey

import "strings"

func EffectiveAPIKeyRowWithProfiles(row APIKeyRow, profiles []PermissionProfileSnapshot) APIKeyRow {
	profileID := strings.TrimSpace(row.PermissionProfileID)
	if profileID == "" {
		return row
	}

	var matched *PermissionProfileSnapshot
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) == profileID {
			copy := profile
			matched = &copy
			break
		}
	}
	if matched == nil {
		return row
	}

	row.PermissionProfileID = profileID
	row.DailyLimit = matched.DailyLimit
	row.TotalQuota = matched.TotalQuota
	row.DailySpendingLimit = matched.DailySpendingLimit
	row.PeriodSpendingLimits = matched.PeriodSpendingLimits
	row.PeriodSpendingLimits.Day = row.DailySpendingLimit
	row.ConcurrencyLimit = matched.ConcurrencyLimit
	row.RPMLimit = matched.RPMLimit
	row.TPMLimit = matched.TPMLimit
	row.AllowedModels = append([]string(nil), matched.AllowedModels...)
	row.AllowedChannels = append([]string(nil), matched.AllowedChannels...)
	row.AllowedChannelGroups = append([]string(nil), matched.AllowedChannelGroups...)
	row.SystemPrompt = matched.SystemPrompt
	return row
}

func EffectiveAPIKeyRowsWithProfiles(rows []APIKeyRow, profiles []PermissionProfileSnapshot) []APIKeyRow {
	if len(rows) == 0 {
		return rows
	}
	out := make([]APIKeyRow, len(rows))
	for idx, row := range rows {
		out[idx] = EffectiveAPIKeyRowWithProfiles(row, profiles)
	}
	return out
}
