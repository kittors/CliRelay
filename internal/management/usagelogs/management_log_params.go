package usagelogs

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// managementLogParams 把管理端日志查询的输入翻译成存储层查询参数：账号代表 Key 展开成
// 账号名下全部 Key，渠道筛选解析成凭据/渠道选择器，按账号查看时用账号的稳定 ID 过滤。
func (s *Service) managementLogParams(input ManagementLogQueryInput, maps nameMaps) usage.LogQueryParams {
	apiKeys := expandManagementAPIKeyFilters(s.tenantID, input.APIKeys)
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
	return usage.LogQueryParams{
		TenantID:              s.tenantID,
		EndUserID:             strings.TrimSpace(input.EndUserID),
		Page:                  input.Page,
		Size:                  input.Size,
		Days:                  input.Days,
		APIKeys:               apiKeys,
		APIKeyIDs:             expandManagementAPIKeyIDs(s.tenantID, input.APIKeyIDs),
		CollapseAccountKeys:   true,
		Models:                input.Models,
		Statuses:              input.Statuses,
		MatchNoAPIKeys:        input.MatchNoAPIKeys,
		MatchNoAPIKeyIDs:      input.MatchNoAPIKeyIDs,
		MatchNoModels:         input.MatchNoModels,
		MatchNoStatuses:       input.MatchNoStatuses,
		MatchNoChannels:       input.MatchNoChannels,
		AuthSubjectIDs:        authSubjectIDs,
		AuthIndexes:           authIndexes,
		ChannelNames:          channelNames,
		AuthIndexChannelNames: authIndexChannelNames,
	}
}
