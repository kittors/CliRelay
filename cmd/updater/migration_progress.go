package main

import (
	"fmt"
	"strconv"
	"strings"
)

type migrationStatus struct {
	Phase          string `json:"phase,omitempty"`
	TargetDatabase string `json:"target_database,omitempty"`
	Table          string `json:"table,omitempty"`
	TableIndex     int    `json:"table_index,omitempty"`
	TableTotal     int    `json:"table_total,omitempty"`
	InsertedRows   int64  `json:"inserted_rows,omitempty"`
	TargetRows     int64  `json:"target_rows,omitempty"`
	PlannedInserts int64  `json:"planned_inserts,omitempty"`
}

func (s *updaterServer) updateProgressFromStage(stage string, message string) {
	switch strings.TrimSpace(stage) {
	case "migrating":
		migration := s.ensureMigrationStatus()
		migration.TargetDatabase = "PostgreSQL"
		switch strings.TrimSpace(message) {
		case "starting PostgreSQL/Redis before SQLite migration":
			migration.Phase = "starting_runtime"
			s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 35)
		case "migrating legacy SQLite data before restarting service":
			migration.Phase = "preparing"
			s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 40)
		case "finishing SQLite migration before service restart":
			migration.Phase = "finalizing"
			s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 90)
		}
	case "restarting":
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 92)
	case "verifying":
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 97)
	}
}

func (s *updaterServer) updateProgressFromLog(message string) {
	if strings.Contains(message, "clirelay sqlite migration: running read-only SQLite inventory") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "inventory"
		migration.TargetDatabase = "PostgreSQL"
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 44)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: running PostgreSQL import dry-run") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "dry_run"
		migration.TargetDatabase = "PostgreSQL"
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 54)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: applying SQLite import into PostgreSQL") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "applying"
		migration.TargetDatabase = "PostgreSQL"
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 62)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: migration complete") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "finalizing"
		migration.TargetDatabase = "PostgreSQL"
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, 90)
		return
	}
	if strings.Contains(message, "sqlite import progress: table ") {
		s.updateSQLiteTableProgress(message)
	}
}

func (s *updaterServer) ensureMigrationStatus() *migrationStatus {
	if s.status.Migration == nil {
		s.status.Migration = &migrationStatus{TargetDatabase: "PostgreSQL"}
	}
	return s.status.Migration
}

func (s *updaterServer) updateSQLiteTableProgress(message string) {
	migration := s.ensureMigrationStatus()
	migration.TargetDatabase = "PostgreSQL"
	_, text, ok := strings.Cut(message, "sqlite import progress: table ")
	if !ok {
		return
	}
	text = strings.TrimSpace(text)
	var index, total int
	var table string
	if n, _ := fmt.Sscanf(text, "%d/%d %s", &index, &total, &table); n == 3 {
		migration.TableIndex = index
		migration.TableTotal = total
		migration.Table = strings.TrimSpace(table)
		if migration.Phase == "" || migration.Phase == "preparing" || migration.Phase == "dry_run" {
			migration.Phase = "applying"
		}
		s.status.ProgressPercent = maxInt(s.status.ProgressPercent, migrationTablePercent(index, total))
		return
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return
	}
	migration.Table = fields[0]
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "inserted_rows":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				migration.InsertedRows = parsed
			}
		case "target_rows":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				migration.TargetRows = parsed
			}
		case "planned_inserts":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				migration.PlannedInserts = parsed
			}
		}
	}
	if strings.Contains(text, "dry-run") {
		migration.Phase = "dry_run"
	} else if migration.Phase == "" || migration.Phase == "preparing" || migration.Phase == "dry_run" {
		migration.Phase = "applying"
	}
}

func progressPercentForStage(stage string) int {
	switch strings.TrimSpace(stage) {
	case "idle":
		return 0
	case "preparing":
		return 5
	case "pulling":
		return 15
	case "migrating":
		return 35
	case "restarting":
		return 92
	case "verifying":
		return 97
	case "completed":
		return 100
	default:
		return 0
	}
}

func migrationTablePercent(index int, total int) int {
	if index <= 0 || total <= 0 {
		return 62
	}
	if index > total {
		index = total
	}
	return 62 + (index * 26 / total)
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
