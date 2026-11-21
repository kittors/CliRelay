package modelconfig

import log "github.com/sirupsen/logrus"

// ModelEnabledState is the part of a model_configs row that the disabled-model
// filter reads.
//
// That filter refreshes each tenant's set every TTL (15 s) on every node, so it
// is by far the most frequent reader of this table. When it read whole rows
// through ListModelConfigs (21 columns, ~475 bytes each) it was the largest
// source of outbound traffic on the database host: about 6 GB a day measured on
// 2026-11-21, almost all of it columns the filter discards.
type ModelEnabledState struct {
	ModelID string
	Enabled bool
}

// ListModelEnabledStates is ListModelConfigs narrowed to model_id and enabled.
//
// It keeps the same filter and order (lower(model_id)), so a caller that merges
// tenants over it sees rows in the sequence ListModelConfigs would produce and
// reaches the same answer.
func (s Store) ListModelEnabledStates() []ModelEnabledState {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT model_id, enabled FROM model_configs WHERE tenant_id = ? ORDER BY lower(model_id)`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/modelconfig: list tenant model enabled states: %v", err)
		return nil
	}
	defer rows.Close()
	result := make([]ModelEnabledState, 0)
	for rows.Next() {
		var state ModelEnabledState
		var enabled int
		if rows.Scan(&state.ModelID, &enabled) == nil {
			state.Enabled = intToBool(enabled)
			result = append(result, state)
		}
	}
	return result
}
