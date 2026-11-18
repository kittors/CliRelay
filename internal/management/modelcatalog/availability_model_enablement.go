package modelcatalog

// Model enablement scoping: which catalog models an operator's enable/disable
// toggles keep in play, both additively (config rows that contribute a model)
// and subtractively (models removed from whatever is about to be served).

import (
	"strings"

	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func mappedOwnerRowModelKeys(rows []usage.ModelConfigRow, ownerKeys map[string]bool) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.ModelID))
		if key == "" || !row.Enabled || !ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] {
			continue
		}
		out[key] = true
	}
	return out
}

// defaultMappedOwnerRows returns the config rows the operator has switched on
// for the default owner scope, plus the key sets the caller needs to merge them.
// It lives here because the Enabled check is the additive half of the same
// toggle the disabled-model filters subtract with.
func (s *Service) defaultMappedOwnerRows() ([]usage.ModelConfigRow, map[string]bool, map[string]bool, bool) {
	ownerKeys := s.defaultMappedOwnerKeys()
	if len(ownerKeys) == 0 {
		return nil, nil, nil, false
	}
	rows := modelconfigsettings.ListAllConfigsForTenant(s.tenantID)
	out := make([]usage.ModelConfigRow, 0, len(rows))
	configuredModelKeys := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if key := strings.ToLower(strings.TrimSpace(row.ModelID)); key != "" {
			configuredModelKeys[key] = true
		}
		if ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] {
			out = append(out, row)
		}
	}
	return out, ownerKeys, configuredModelKeys, true
}
