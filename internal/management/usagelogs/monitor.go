package usagelogs

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// MonitorQuery is the parsed filter of the monitor center endpoints.
type MonitorQuery struct {
	Range     string
	Consumers []string
	Models    []string
	Channels  []string
}

// MonitorOverview returns the monitor center payload scoped to the service's
// tenant. Channel provider badges come from the live AI account when it still
// exists; the usage layer can only guess them from the channel name.
func (s *Service) MonitorOverview(q MonitorQuery) (usage.MonitorOverview, error) {
	overview, err := usage.QueryMonitorOverview(s.monitorFilter(q), q.Range, time.Now())
	if err != nil {
		return overview, err
	}
	metaBySubject := s.monitorAuthMetaBySubject()
	if len(metaBySubject) == 0 {
		return overview, nil
	}
	for i := range overview.Channels.Rows {
		row := &overview.Channels.Rows[i]
		if meta, ok := metaBySubject[row.AuthSubjectID]; ok {
			row.Provider, row.AuthType = preferNonEmpty(meta.provider, row.Provider), preferNonEmpty(meta.authType, row.AuthType)
		}
	}
	for i := range overview.Filters.Channels {
		option := &overview.Filters.Channels[i]
		if meta, ok := metaBySubject[option.AuthSubjectID]; ok {
			option.Provider = preferNonEmpty(meta.provider, option.Provider)
		}
	}
	return overview, nil
}

// MonitorRealtime returns the last hour at one-minute resolution.
func (s *Service) MonitorRealtime(q MonitorQuery) (usage.MonitorRealtime, error) {
	return usage.QueryMonitorRealtime(s.monitorFilter(q), time.Now())
}

func (s *Service) monitorFilter(q MonitorQuery) usage.MonitorFilter {
	endUserIDs, apiKeyIDs := usage.ParseMonitorConsumers(q.Consumers)
	return usage.MonitorFilter{
		TenantID:   s.tenantID,
		EndUserIDs: endUserIDs,
		APIKeyIDs:  apiKeyIDs,
		Models:     q.Models,
		Channels:   q.Channels,
	}
}

// monitorAuthMetaBySubject indexes the tenant's live AI accounts by subject.
// Enabled accounts win over disabled ones sharing a subject, matching how the
// request log channel options pick their representative.
func (s *Service) monitorAuthMetaBySubject() map[string]authChannelMeta {
	out := make(map[string]authChannelMeta)
	if s == nil || s.authManager == nil {
		return out
	}
	for _, auth := range s.authManager.ListForTenant(s.tenantID) {
		if auth == nil {
			continue
		}
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || strings.TrimSpace(identity.ID) == "" {
			continue
		}
		subjectID := strings.TrimSpace(identity.ID)
		if _, exists := out[subjectID]; exists && auth.Disabled {
			continue
		}
		out[subjectID] = authChannelMeta{
			label:    strings.TrimSpace(auth.ChannelName()),
			provider: normalizeProviderKey(auth.Provider),
			authType: resolveAuthType(auth),
		}
	}
	return out
}

func preferNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}
