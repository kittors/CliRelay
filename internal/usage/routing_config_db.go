package usage

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const createRoutingConfigTableSQL = `
CREATE TABLE IF NOT EXISTS routing_config (
  id         INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
  payload    TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT ''
);
`

func initRoutingConfigTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createRoutingConfigTableSQL); err != nil {
		log.Errorf("usage: create routing_config table: %v", err)
	}
}

func normalizeRoutingConfig(input config.RoutingConfig) config.RoutingConfig {
	holder := &config.Config{Routing: input}
	holder.SanitizeRouting()
	return holder.Routing
}

func routingConfigMeaningful(cfg config.RoutingConfig) bool {
	return cfg.Strategy != "" || !cfg.IncludeDefaultGroup || len(cfg.ChannelGroups) > 0 || len(cfg.PathRoutes) > 0
}

func ApplyStoredRoutingConfig(cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	stored := GetRoutingConfig()
	if stored == nil {
		return false
	}
	cfg.Routing = normalizeRoutingConfig(*stored)
	return true
}

func MigrateRoutingConfigFromConfig(cfg *config.Config, configFilePath string) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	if GetRoutingConfig() != nil {
		cleanRoutingConfigFromYAML(configFilePath)
		return false
	}
	if !routingConfigMeaningful(cfg.Routing) {
		return false
	}
	if err := UpsertRoutingConfig(cfg.Routing); err != nil {
		log.Errorf("usage: migrate routing config: %v", err)
		return false
	}
	if strings.TrimSpace(configFilePath) != "" {
		if backupConfigForMigration(configFilePath, routingMigrationBackupSuffix) {
			cleanRoutingConfigFromYAML(configFilePath)
		}
	}
	return true
}

func GetRoutingConfig() *config.RoutingConfig {
	db := getDB()
	if db == nil {
		return nil
	}

	var payload string
	if err := db.QueryRow(`SELECT payload FROM routing_config WHERE id = 1`).Scan(&payload); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("usage: load routing_config: %v", err)
		}
		return nil
	}

	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}

	var cfg config.RoutingConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		log.Warnf("usage: decode routing_config: %v", err)
		return nil
	}
	normalized := normalizeRoutingConfig(cfg)
	return &normalized
}

func UpsertRoutingConfig(cfg config.RoutingConfig) error {
	db := getDB()
	if db == nil {
		return nil
	}

	normalized := normalizeRoutingConfig(cfg)
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}

	_, err = db.Exec(
		`INSERT INTO routing_config (id, payload, updated_at)
		 VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		string(payload),
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}
