package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type CcSwitchImportConfigRow struct {
	ID                   string   `json:"id"`
	ClientType           string   `json:"client-type"`
	ProviderName         string   `json:"provider-name"`
	Note                 string   `json:"note"`
	DefaultModel         string   `json:"default-model"`
	AllowedChannelGroups []string `json:"allowed-channel-groups"`
	EndpointPath         string   `json:"endpoint-path"`
	UsageAutoInterval    int      `json:"usage-auto-interval"`
	APIKeyField          string   `json:"api-key-field,omitempty"`
	CreatedAt            string   `json:"created-at,omitempty"`
	UpdatedAt            string   `json:"updated-at,omitempty"`
}

const createCcSwitchImportConfigsTableSQL = `
CREATE TABLE IF NOT EXISTS ccswitch_import_configs (
  id                     TEXT PRIMARY KEY NOT NULL,
  client_type            TEXT NOT NULL,
  provider_name          TEXT NOT NULL DEFAULT '',
  note                   TEXT NOT NULL DEFAULT '',
  default_model          TEXT NOT NULL DEFAULT '',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
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
}

func ListCcSwitchImportConfigs() []CcSwitchImportConfigRow {
	db := getDB()
	if db == nil {
		return nil
	}

	rows, err := db.Query(`SELECT id, client_type, provider_name, note, default_model,
		allowed_channel_groups, endpoint_path, usage_auto_interval, api_key_field, created_at, updated_at
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
		(id, client_type, provider_name, note, default_model, allowed_channel_groups,
		 endpoint_path, usage_auto_interval, api_key_field, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(configs))
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
			mustJSONStringList(row.AllowedChannelGroups),
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
	row.AllowedChannelGroups = normalizeLowerStringSlice(row.AllowedChannelGroups)
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

func normalizeCcSwitchAPIKeyField(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "ANTHROPIC_AUTH_TOKEN") {
		return "ANTHROPIC_AUTH_TOKEN"
	}
	return "ANTHROPIC_API_KEY"
}

func scanCcSwitchImportConfigFromRow(row scannable) *CcSwitchImportConfigRow {
	var result CcSwitchImportConfigRow
	var allowedChannelGroupsJSON string
	if err := row.Scan(
		&result.ID,
		&result.ClientType,
		&result.ProviderName,
		&result.Note,
		&result.DefaultModel,
		&allowedChannelGroupsJSON,
		&result.EndpointPath,
		&result.UsageAutoInterval,
		&result.APIKeyField,
		&result.CreatedAt,
		&result.UpdatedAt,
	); err != nil {
		return nil
	}
	result.AllowedChannelGroups = decodeJSONStringList(allowedChannelGroupsJSON)
	return &result
}
