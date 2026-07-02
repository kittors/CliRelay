package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	log "github.com/sirupsen/logrus"
)

type CcSwitchModelMappingRow struct {
	Role         string `json:"role,omitempty"`
	RequestModel string `json:"request-model"`
	TargetModel  string `json:"target-model"`
}

type CcSwitchImportConfigRow struct {
	ID                        string                     `json:"id"`
	ClientType                string                     `json:"client-type"`
	ProviderName              string                     `json:"provider-name"`
	Note                      string                     `json:"note"`
	DefaultModel              string                     `json:"default-model"`
	ModelMappings             []CcSwitchModelMappingRow  `json:"model-mappings"`
	AllowedChannelGroups      []string                   `json:"allowed-channel-groups"`
	RoutePath                 string                     `json:"route-path,omitempty"`
	EndpointPath              string                     `json:"endpoint-path"`
	UsageAutoInterval         int                        `json:"usage-auto-interval"`
	APIKeyField               string                     `json:"api-key-field,omitempty"`
	CodexModelCatalogFilename string                     `json:"codex-model-catalog-filename,omitempty"`
	CodexModelCatalog         *CcSwitchCodexModelCatalog `json:"codex-model-catalog,omitempty"`
	CreatedAt                 string                     `json:"created-at,omitempty"`
	UpdatedAt                 string                     `json:"updated-at,omitempty"`
}

const createCcSwitchImportConfigsTableSQL = `
CREATE TABLE IF NOT EXISTS ccswitch_import_configs (
  id                     TEXT PRIMARY KEY NOT NULL,
  client_type            TEXT NOT NULL,
  provider_name          TEXT NOT NULL DEFAULT '',
  note                   TEXT NOT NULL DEFAULT '',
  default_model          TEXT NOT NULL DEFAULT '',
  model_mappings         TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  route_path             TEXT NOT NULL DEFAULT '',
  endpoint_path          TEXT NOT NULL DEFAULT '',
  usage_auto_interval    INTEGER NOT NULL DEFAULT 30,
  api_key_field          TEXT NOT NULL DEFAULT '',
  created_at             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL DEFAULT ''
);
`

func initCcSwitchImportConfigsTable(db *sql.DB) {
	if _, err := db.Exec(createCcSwitchImportConfigsTableSQL); err != nil {
		log.Errorf("usage: create ccswitch_import_configs table: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE ccswitch_import_configs ADD COLUMN model_mappings TEXT NOT NULL DEFAULT '[]'`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Errorf("usage: migrate ccswitch_import_configs.model_mappings: %v", err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE ccswitch_import_configs ADD COLUMN route_path TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Errorf("usage: migrate ccswitch_import_configs.route_path: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_ccswitch_import_configs_route_path
		ON ccswitch_import_configs(route_path) WHERE route_path <> ''`); err != nil {
		log.Errorf("usage: create ccswitch_import_configs.route_path index: %v", err)
	}
}

func ListCcSwitchImportConfigs() []CcSwitchImportConfigRow {
	db := getDB()
	if db == nil {
		return nil
	}

	rows, err := db.Query(`SELECT id, client_type, provider_name, note, default_model, model_mappings,
		allowed_channel_groups, route_path, endpoint_path, usage_auto_interval, api_key_field, created_at, updated_at
		FROM ccswitch_import_configs ORDER BY created_at ASC, id ASC`)
	if err != nil {
		log.Errorf("usage: list ccswitch_import_configs: %v", err)
		return nil
	}
	defer rows.Close()

	var result []CcSwitchImportConfigRow
	for rows.Next() {
		row := scanCcSwitchImportConfigFromRow(rows)
		if row != nil {
			result = append(result, *row)
		}
	}
	return result
}

func FindCcSwitchImportConfigByRoutePath(routePath string) (CcSwitchImportConfigRow, bool) {
	db := getDB()
	if db == nil {
		return CcSwitchImportConfigRow{}, false
	}
	normalizedRoutePath := normalizeCcSwitchRoutePath(routePath)
	if normalizedRoutePath == "" {
		return CcSwitchImportConfigRow{}, false
	}

	row := db.QueryRow(`SELECT id, client_type, provider_name, note, default_model, model_mappings,
		allowed_channel_groups, route_path, endpoint_path, usage_auto_interval, api_key_field, created_at, updated_at
		FROM ccswitch_import_configs WHERE route_path = ? LIMIT 1`, normalizedRoutePath)
	result := scanCcSwitchImportConfigFromRow(row)
	if result == nil {
		return CcSwitchImportConfigRow{}, false
	}
	return *result, true
}

func ReplaceAllCcSwitchImportConfigs(configs []CcSwitchImportConfigRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM ccswitch_import_configs"); err != nil {
		_ = tx.Rollback()
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO ccswitch_import_configs
		(id, client_type, provider_name, note, default_model, model_mappings, allowed_channel_groups,
		 route_path, endpoint_path, usage_auto_interval, api_key_field, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(configs))
	seenRoutePaths := make(map[string]struct{}, len(configs))
	for _, row := range configs {
		row = normalizeCcSwitchImportConfigRow(row)
		if row.ID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("id is required")
		}
		if row.ClientType == "" {
			_ = tx.Rollback()
			return fmt.Errorf("client-type is required")
		}
		if row.ProviderName == "" {
			_ = tx.Rollback()
			return fmt.Errorf("provider-name is required")
		}
		if row.DefaultModel == "" {
			_ = tx.Rollback()
			return fmt.Errorf("default-model is required")
		}
		if _, exists := seen[row.ID]; exists {
			_ = tx.Rollback()
			return fmt.Errorf("duplicate id %q", row.ID)
		}
		seen[row.ID] = struct{}{}
		if row.RoutePath != "" {
			if _, exists := seenRoutePaths[row.RoutePath]; exists {
				_ = tx.Rollback()
				return fmt.Errorf("duplicate route-path %q", row.RoutePath)
			}
			seenRoutePaths[row.RoutePath] = struct{}{}
		}
		if row.CreatedAt == "" {
			row.CreatedAt = now
		}
		row.UpdatedAt = now

		if _, err := stmt.Exec(
			row.ID,
			row.ClientType,
			row.ProviderName,
			row.Note,
			row.DefaultModel,
			mustJSONModelMappings(row.ModelMappings),
			mustJSONStringList(row.AllowedChannelGroups),
			row.RoutePath,
			row.EndpointPath,
			row.UsageAutoInterval,
			row.APIKeyField,
			row.CreatedAt,
			row.UpdatedAt,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func normalizeCcSwitchImportConfigRow(row CcSwitchImportConfigRow) CcSwitchImportConfigRow {
	row.ID = strings.TrimSpace(row.ID)
	row.ClientType = strings.ToLower(strings.TrimSpace(row.ClientType))
	row.ProviderName = strings.TrimSpace(row.ProviderName)
	row.Note = strings.TrimSpace(row.Note)
	row.DefaultModel = strings.TrimSpace(row.DefaultModel)
	row.ModelMappings = normalizeCcSwitchModelMappings(row.ModelMappings)
	row.AllowedChannelGroups = normalizeLowerStringSlice(row.AllowedChannelGroups)
	row.RoutePath = normalizeCcSwitchRoutePath(row.RoutePath)
	row.EndpointPath = normalizeCcSwitchEndpointPath(row.EndpointPath)
	if row.UsageAutoInterval <= 0 {
		row.UsageAutoInterval = 30
	}
	if row.ClientType == "claude" {
		row.APIKeyField = normalizeCcSwitchAPIKeyField(row.APIKeyField)
	} else {
		row.APIKeyField = ""
	}
	return row
}

func normalizeLowerStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if result == nil {
		return []string{}
	}
	return result
}

func normalizeCcSwitchEndpointPath(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" || raw == "/" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return strings.TrimRight(raw, "/")
}

func normalizeCcSwitchRoutePath(value string) string {
	return internalrouting.NormalizeNamespacePath(value)
}

func normalizeCcSwitchAPIKeyField(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "ANTHROPIC_AUTH_TOKEN") {
		return "ANTHROPIC_AUTH_TOKEN"
	}
	return "ANTHROPIC_API_KEY"
}

func normalizeCcSwitchModelMappings(values []CcSwitchModelMappingRow) []CcSwitchModelMappingRow {
	if len(values) == 0 {
		return []CcSwitchModelMappingRow{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]CcSwitchModelMappingRow, 0, len(values))
	for _, value := range values {
		role := normalizeCcSwitchModelRole(value.Role)
		requestModel := strings.TrimSpace(value.RequestModel)
		targetModel := strings.TrimSpace(value.TargetModel)
		if targetModel == "" {
			continue
		}
		if requestModel == "" {
			requestModel = targetModel
		}
		key := strings.ToLower(requestModel) + "::" + strings.ToLower(targetModel)
		if role != "" {
			key = "role:" + role
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, CcSwitchModelMappingRow{
			Role:         role,
			RequestModel: requestModel,
			TargetModel:  targetModel,
		})
	}
	if result == nil {
		return []CcSwitchModelMappingRow{}
	}
	return result
}

func normalizeCcSwitchModelRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "main", "haiku", "sonnet", "opus":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func mustJSONModelMappings(values []CcSwitchModelMappingRow) string {
	data, err := json.Marshal(normalizeCcSwitchModelMappings(values))
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeJSONModelMappings(raw string) []CcSwitchModelMappingRow {
	var values []CcSwitchModelMappingRow
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []CcSwitchModelMappingRow{}
	}
	return normalizeCcSwitchModelMappings(values)
}

func scanCcSwitchImportConfigFromRow(row scannable) *CcSwitchImportConfigRow {
	var result CcSwitchImportConfigRow
	var modelMappingsJSON string
	var allowedChannelGroupsJSON string
	if err := row.Scan(
		&result.ID,
		&result.ClientType,
		&result.ProviderName,
		&result.Note,
		&result.DefaultModel,
		&modelMappingsJSON,
		&allowedChannelGroupsJSON,
		&result.RoutePath,
		&result.EndpointPath,
		&result.UsageAutoInterval,
		&result.APIKeyField,
		&result.CreatedAt,
		&result.UpdatedAt,
	); err != nil {
		return nil
	}
	result.ModelMappings = decodeJSONModelMappings(modelMappingsJSON)
	result.AllowedChannelGroups = decodeJSONStringList(allowedChannelGroupsJSON)
	return &result
}
