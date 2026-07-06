package main

import (
	"fmt"
	"strconv"
	"strings"
)

type migrationStatus struct {
	Phase          string `json:"phase,omitempty"`
	TargetDatabase string `json:"target_database,omitempty"`
	SkipReason     string `json:"skip_reason,omitempty"`
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
		case "starting PostgreSQL/Redis before SQLite migration", "starting PostgreSQL/Redis before data migration check":
			migration.Phase = "starting_runtime"
			s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 35)
		case "checking legacy SQLite migration before service restart":
			migration.Phase = "checking"
			s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 40)
		case "legacy SQLite migration check finished before service restart", "finishing SQLite migration before service restart":
			if migration.Phase == "skipped" {
				s.status.Message = migrationSkippedMessage(migration.SkipReason)
				s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 90)
				return
			}
			migration.Phase = "finalizing"
			s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 90)
		}
	case "restarting":
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 92)
	case "verifying":
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 97)
	}
}

func (s *updaterServer) updateProgressFromLog(message string) {
	if strings.Contains(message, "clirelay sqlite migration: disabled by CLIRELAY_SQLITE_AUTO_MIGRATE") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "skipped"
		migration.SkipReason = "disabled"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "legacy SQLite migration skipped because auto-migration is disabled"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 88)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: no legacy usage.db found") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "skipped"
		migration.SkipReason = "no_legacy_sqlite"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "no legacy SQLite database found; continuing with PostgreSQL runtime data"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 88)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: apply disabled by CLIRELAY_SQLITE_AUTO_IMPORT") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "skipped"
		migration.SkipReason = "import_disabled"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "SQLite import dry-run complete; apply is disabled"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 88)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: legacy SQLite found at ") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "preparing"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "legacy SQLite database found; preparing PostgreSQL import"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 42)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: running read-only SQLite inventory") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "inventory"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "running SQLite inventory before PostgreSQL import"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 44)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: running PostgreSQL import dry-run") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "dry_run"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "running PostgreSQL import dry-run"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 54)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: applying SQLite import into PostgreSQL") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "applying"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "applying legacy SQLite data into PostgreSQL"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 62)
		return
	}
	if strings.Contains(message, "clirelay sqlite migration: migration complete") {
		migration := s.ensureMigrationStatus()
		migration.Phase = "finalizing"
		migration.TargetDatabase = "PostgreSQL"
		s.status.Message = "legacy SQLite migration complete; preparing service restart"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, 90)
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

func migrationSkippedMessage(reason string) string {
	switch reason {
	case "disabled":
		return "legacy SQLite migration skipped because auto-migration is disabled"
	case "no_legacy_sqlite":
		return "no legacy SQLite database found; continuing with PostgreSQL runtime data"
	case "import_disabled":
		return "SQLite import dry-run complete; apply is disabled"
	default:
		return "legacy SQLite migration check finished before service restart"
	}
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
		if migration.Phase == "" || migration.Phase == "checking" || migration.Phase == "preparing" {
			migration.Phase = "applying"
		}
		s.status.Message = "applying legacy SQLite data into PostgreSQL"
		s.status.ProgressPercent = maxFloat(s.status.ProgressPercent, migrationTableStartPercent(index, total))
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
	if migration.TableIndex > 0 && migration.TableTotal > 0 && migration.TargetRows > 0 {
		s.status.ProgressPercent = maxFloat(
			s.status.ProgressPercent,
			migrationTableRowPercent(
				migration.TableIndex,
				migration.TableTotal,
				migration.InsertedRows,
				migration.TargetRows,
			),
		)
	}
	if strings.Contains(text, "dry-run") {
		migration.Phase = "dry_run"
	} else if migration.Phase == "" || migration.Phase == "checking" || migration.Phase == "preparing" || migration.Phase == "dry_run" {
		migration.Phase = "applying"
	}
	if migration.Phase == "applying" {
		s.status.Message = "applying legacy SQLite data into PostgreSQL"
	}
}

func progressPercentForStage(stage string) float64 {
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

func migrationTableStartPercent(index int, total int) float64 {
	if index <= 0 || total <= 0 {
		return 62
	}
	if index > total {
		index = total
	}
	return 62 + (float64(index-1) * 26 / float64(total))
}

func migrationTableRowPercent(index int, total int, inserted int64, target int64) float64 {
	if index <= 0 || total <= 0 || target <= 0 {
		return migrationTableStartPercent(index, total)
	}
	if index > total {
		index = total
	}
	if inserted < 0 {
		inserted = 0
	}
	if inserted > target {
		inserted = target
	}
	completed := float64(index - 1)
	fraction := float64(inserted) / float64(target)
	return 62 + ((completed + fraction) * 26 / float64(total))
}

func maxFloat(left float64, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
