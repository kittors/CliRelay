package usage

import "strings"

// MonitorFilter narrows every monitor query. Values inside one dimension are
// OR-ed and dimensions are AND-ed. A consumer is either a portal end user or a
// standalone API key; picking both kinds ORs them, because a reader selecting
// "Alice" and "the CI key" wants the traffic of both.
type MonitorFilter struct {
	TenantID   string
	EndUserIDs []string
	APIKeyIDs  []string
	Models     []string
	Channels   []string
}

const (
	monitorConsumerEndUserPrefix = "eu:"
	monitorConsumerAPIKeyPrefix  = "key:"
)

// ParseMonitorConsumers splits "eu:<id>" / "key:<id>" selectors. Anything else
// is dropped: a raw secret must never reach a query as a selector.
func ParseMonitorConsumers(values []string) (endUserIDs, apiKeyIDs []string) {
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(value, monitorConsumerEndUserPrefix):
			endUserIDs = append(endUserIDs, strings.TrimPrefix(value, monitorConsumerEndUserPrefix))
		case strings.HasPrefix(value, monitorConsumerAPIKeyPrefix):
			apiKeyIDs = append(apiKeyIDs, strings.TrimPrefix(value, monitorConsumerAPIKeyPrefix))
		}
	}
	return dedupeExactStrings(endUserIDs), dedupeExactStrings(apiKeyIDs)
}

// monitorConsumerKey folds a rollup identity into one consumer: keys owned by
// a portal user count toward that user, others stand on their own.
func monitorConsumerKey(endUserID, apiKeyID string) string {
	if eu := strings.TrimSpace(endUserID); eu != "" {
		return monitorConsumerEndUserPrefix + eu
	}
	if id := strings.TrimSpace(apiKeyID); id != "" {
		return monitorConsumerAPIKeyPrefix + id
	}
	return ""
}

func (f MonitorFilter) normalized() MonitorFilter {
	f.TenantID = normalizeTenantID(f.TenantID)
	f.EndUserIDs = dedupeExactStrings(f.EndUserIDs)
	f.APIKeyIDs = dedupeExactStrings(f.APIKeyIDs)
	f.Models = dedupeExactStrings(f.Models)
	f.Channels = dedupeLowerTrimmedStrings(f.Channels)
	return f
}

func (f MonitorFilter) active() bool {
	return len(f.EndUserIDs) > 0 || len(f.APIKeyIDs) > 0 || len(f.Models) > 0 || len(f.Channels) > 0
}

// unfiltered keeps only the tenant: filter options must list every value in
// the window, or narrowing one dimension would hide the way back.
func (f MonitorFilter) unfiltered() MonitorFilter {
	return MonitorFilter{TenantID: f.TenantID}
}

// rollupPredicate renders the filter for usage_rollup_buckets as " AND ..."
// clauses (empty when nothing is selected).
func (f MonitorFilter) rollupPredicate() (string, []any) {
	var b strings.Builder
	var args []any
	var consumer []string
	if len(f.EndUserIDs) > 0 {
		consumer = append(consumer, "end_user_id IN ("+placeholders(len(f.EndUserIDs))+")")
		for _, id := range f.EndUserIDs {
			args = append(args, id)
		}
	}
	if len(f.APIKeyIDs) > 0 {
		consumer = append(consumer, "api_key_id IN ("+placeholders(len(f.APIKeyIDs))+")")
		for _, id := range f.APIKeyIDs {
			args = append(args, id)
		}
	}
	if len(consumer) > 0 {
		b.WriteString(" AND (" + strings.Join(consumer, " OR ") + ")")
	}
	args = f.appendModelChannel(&b, args)
	return b.String(), args
}

// detailPredicate renders the filter for request_logs, which has no
// end_user_id column: end users expand to the ids of the keys they own. A
// selected end user without keys matches nothing instead of silently widening
// the query to the whole tenant.
func (f MonitorFilter) detailPredicate() (string, []any) {
	var b strings.Builder
	var args []any
	if len(f.EndUserIDs) > 0 || len(f.APIKeyIDs) > 0 {
		keyIDs := append([]string{}, f.APIKeyIDs...)
		for _, eu := range f.EndUserIDs {
			keyIDs = append(keyIDs, ListAPIKeyIDsForEndUserForTenant(f.TenantID, eu)...)
		}
		keyIDs = dedupeExactStrings(keyIDs)
		if len(keyIDs) == 0 {
			b.WriteString(" AND 1 = 0")
		} else {
			b.WriteString(" AND api_key_id IN (" + placeholders(len(keyIDs)) + ")")
			for _, id := range keyIDs {
				args = append(args, id)
			}
		}
	}
	args = f.appendModelChannel(&b, args)
	return b.String(), args
}

func (f MonitorFilter) appendModelChannel(b *strings.Builder, args []any) []any {
	if len(f.Models) > 0 {
		b.WriteString(" AND model IN (" + placeholders(len(f.Models)) + ")")
		for _, model := range f.Models {
			args = append(args, model)
		}
	}
	if len(f.Channels) > 0 {
		// Channel names are matched case-insensitively, like the request log
		// filters, so a renamed-case channel does not split.
		b.WriteString(" AND lower(trim(channel_name)) IN (" + placeholders(len(f.Channels)) + ")")
		for _, channel := range f.Channels {
			args = append(args, channel)
		}
	}
	return args
}
