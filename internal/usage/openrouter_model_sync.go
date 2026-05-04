package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const openRouterModelSource = "openrouter"
const defaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models?output_modalities=all"
const defaultOpenRouterModelSyncIntervalMinutes = 24 * 60
const minOpenRouterModelSyncIntervalMinutes = 60

type OpenRouterRemotePricing struct {
	Prompt         string `json:"prompt"`
	Completion     string `json:"completion"`
	InputCacheRead string `json:"input_cache_read"`
}

type OpenRouterRemoteModel struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Pricing     OpenRouterRemotePricing `json:"pricing"`
}

type OpenRouterModelSyncResult struct {
	Seen    int `json:"seen"`
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

type OpenRouterModelSyncState struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	LastSyncAt      string `json:"last_sync_at"`
	LastSuccessAt   string `json:"last_success_at"`
	LastError       string `json:"last_error"`
	LastSeen        int    `json:"last_seen"`
	LastAdded       int    `json:"last_added"`
	LastUpdated     int    `json:"last_updated"`
	LastSkipped     int    `json:"last_skipped"`
	UpdatedAt       string `json:"updated_at"`
	Running         bool   `json:"running"`
}

type openRouterModelsResponse struct {
	Data []OpenRouterRemoteModel `json:"data"`
}

type OpenRouterModelFetcher func(ctx context.Context) ([]OpenRouterRemoteModel, error)

var (
	openRouterModelFetcherMu sync.RWMutex
	openRouterModelFetcher   OpenRouterModelFetcher = fetchOpenRouterModels

	openRouterSyncRunning       atomic.Bool
	openRouterSyncSchedulerOnce sync.Once
)

func SetOpenRouterModelFetcherForTest(fetcher OpenRouterModelFetcher) func() {
	openRouterModelFetcherMu.Lock()
	previous := openRouterModelFetcher
	openRouterModelFetcher = fetcher
	openRouterModelFetcherMu.Unlock()
	return func() {
		openRouterModelFetcherMu.Lock()
		openRouterModelFetcher = previous
		openRouterModelFetcherMu.Unlock()
	}
}

func SyncOpenRouterModelList(ctx context.Context, models []OpenRouterRemoteModel) (OpenRouterModelSyncResult, error) {
	result := OpenRouterModelSyncResult{Seen: len(models)}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		remoteModelID := strings.TrimSpace(model.ID)
		if remoteModelID == "" {
			result.Skipped++
			continue
		}
		owner := openRouterOwnerFromModelID(remoteModelID)
		modelID := openRouterLocalModelID(remoteModelID, owner)
		if modelID == "" {
			result.Skipped++
			continue
		}
		legacyModelIDs := openRouterLegacyLocalModelIDs(remoteModelID, owner, modelID)
		if existing, exists := GetModelConfig(modelID); exists {
			openRouterApplyModelSync(&existing, model, owner)
			if err := UpsertModelConfig(existing); err != nil {
				return result, fmt.Errorf("sync openrouter model pricing %s: %w", modelID, err)
			}
			if err := openRouterDeleteLegacyOpenRouterRows(modelID, legacyModelIDs); err != nil {
				return result, err
			}
			if err := openRouterSyncExistingAliasRows(modelID, model, owner, legacyModelIDs); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}
		migrated, err := openRouterMigrateLegacyOpenRouterRow(modelID, owner, model, legacyModelIDs)
		if err != nil {
			return result, err
		}
		if migrated {
			if err := openRouterSyncExistingAliasRows(modelID, model, owner, legacyModelIDs); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}

		row := ModelConfigRow{
			ModelID:               modelID,
			OwnedBy:               owner,
			Description:           openRouterModelDescription(model),
			Enabled:               true,
			PricingMode:           "token",
			InputPricePerMillion:  openRouterPricePerMillion(model.Pricing.Prompt),
			OutputPricePerMillion: openRouterPricePerMillion(model.Pricing.Completion),
			CachedPricePerMillion: openRouterPricePerMillion(model.Pricing.InputCacheRead),
			Source:                openRouterModelSource,
		}
		if err := UpsertModelConfig(row); err != nil {
			return result, fmt.Errorf("sync openrouter model %s: %w", modelID, err)
		}
		if err := openRouterSyncExistingAliasRows(modelID, model, owner, legacyModelIDs); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

func GetOpenRouterModelSyncState() OpenRouterModelSyncState {
	db := getDB()
	state := OpenRouterModelSyncState{
		IntervalMinutes: defaultOpenRouterModelSyncIntervalMinutes,
		Running:         openRouterSyncRunning.Load(),
	}
	if db == nil {
		return state
	}
	ensureOpenRouterModelSyncStateRow()
	var enabled int
	if err := db.QueryRow(
		`SELECT enabled, interval_minutes, last_sync_at, last_success_at, last_error, last_seen, last_added, last_updated, last_skipped, updated_at
		 FROM model_openrouter_sync_state WHERE id = 1`,
	).Scan(
		&enabled,
		&state.IntervalMinutes,
		&state.LastSyncAt,
		&state.LastSuccessAt,
		&state.LastError,
		&state.LastSeen,
		&state.LastAdded,
		&state.LastUpdated,
		&state.LastSkipped,
		&state.UpdatedAt,
	); err != nil {
		return state
	}
	state.Enabled = intToBool(enabled)
	state.IntervalMinutes = normalizeOpenRouterModelSyncInterval(state.IntervalMinutes)
	state.Running = openRouterSyncRunning.Load()
	return state
}

func UpdateOpenRouterModelSyncSettings(enabled bool, intervalMinutes int) (OpenRouterModelSyncState, error) {
	db := getDB()
	if db == nil {
		return OpenRouterModelSyncState{}, fmt.Errorf("usage: database not initialised")
	}
	ensureOpenRouterModelSyncStateRow()
	_, err := db.Exec(
		`UPDATE model_openrouter_sync_state
		 SET enabled = ?, interval_minutes = ?, updated_at = ?
		 WHERE id = 1`,
		boolToInt(enabled),
		normalizeOpenRouterModelSyncInterval(intervalMinutes),
		nowRFC3339(),
	)
	if err != nil {
		return OpenRouterModelSyncState{}, fmt.Errorf("usage: update openrouter sync settings: %w", err)
	}
	return GetOpenRouterModelSyncState(), nil
}

func RunOpenRouterModelSync(ctx context.Context) (OpenRouterModelSyncResult, OpenRouterModelSyncState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !openRouterSyncRunning.CompareAndSwap(false, true) {
		state := GetOpenRouterModelSyncState()
		return OpenRouterModelSyncResult{}, state, fmt.Errorf("usage: openrouter model sync already running")
	}
	defer openRouterSyncRunning.Store(false)

	openRouterModelFetcherMu.RLock()
	fetcher := openRouterModelFetcher
	openRouterModelFetcherMu.RUnlock()
	if fetcher == nil {
		fetcher = fetchOpenRouterModels
	}

	models, err := fetcher(ctx)
	if err != nil {
		state := recordOpenRouterModelSyncResult(OpenRouterModelSyncResult{}, err)
		return OpenRouterModelSyncResult{}, state, err
	}
	result, err := SyncOpenRouterModelList(ctx, models)
	state := recordOpenRouterModelSyncResult(result, err)
	return result, state, err
}

func StartOpenRouterModelSyncScheduler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	openRouterSyncSchedulerOnce.Do(func() {
		go runOpenRouterModelSyncScheduler(ctx)
	})
}

func runOpenRouterModelSyncScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	runIfDue := func() {
		state := GetOpenRouterModelSyncState()
		if !state.Enabled || !isOpenRouterModelSyncDue(state, time.Now().UTC()) {
			return
		}
		syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if _, _, err := RunOpenRouterModelSync(syncCtx); err != nil {
			log.Warnf("usage: scheduled openrouter model sync failed: %v", err)
		}
	}

	runIfDue()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runIfDue()
		}
	}
}

func fetchOpenRouterModels(ctx context.Context) ([]OpenRouterRemoteModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultOpenRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CliRelay OpenRouter model sync")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter models request failed: %s", resp.Status)
	}

	var payload openRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func ensureOpenRouterModelSyncStateRow() {
	db := getDB()
	if db == nil {
		return
	}
	ensureOpenRouterModelSyncStateSchema(db)
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO model_openrouter_sync_state
		 (id, enabled, interval_minutes, last_sync_at, last_success_at, last_error, last_seen, last_added, last_updated, last_skipped, updated_at)
		 VALUES (1, 0, ?, '', '', '', 0, 0, 0, 0, ?)`,
		defaultOpenRouterModelSyncIntervalMinutes,
		nowRFC3339(),
	)
}

func ensureOpenRouterModelSyncStateSchema(db *sql.DB) {
	if db == nil || sqliteColumnExists(db, "model_openrouter_sync_state", "last_updated") {
		return
	}
	if _, err := db.Exec("ALTER TABLE model_openrouter_sync_state ADD COLUMN last_updated INTEGER NOT NULL DEFAULT 0"); err != nil {
		log.Warnf("usage: add openrouter sync last_updated column: %v", err)
	}
}

func sqliteColumnExists(db *sql.DB, tableName, columnName string) bool {
	rows, err := db.Query("PRAGMA table_info(" + tableName + ")")
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			continue
		}
		if name == columnName {
			return true
		}
	}
	return false
}

func recordOpenRouterModelSyncResult(result OpenRouterModelSyncResult, syncErr error) OpenRouterModelSyncState {
	db := getDB()
	if db == nil {
		return GetOpenRouterModelSyncState()
	}
	ensureOpenRouterModelSyncStateRow()
	now := nowRFC3339()
	state := GetOpenRouterModelSyncState()
	lastSuccessAt := state.LastSuccessAt
	lastError := ""
	if syncErr != nil {
		lastError = syncErr.Error()
	} else {
		lastSuccessAt = now
	}
	_, _ = db.Exec(
		`UPDATE model_openrouter_sync_state
		 SET last_sync_at = ?, last_success_at = ?, last_error = ?, last_seen = ?, last_added = ?, last_updated = ?, last_skipped = ?, updated_at = ?
		 WHERE id = 1`,
		now,
		lastSuccessAt,
		lastError,
		result.Seen,
		result.Added,
		result.Updated,
		result.Skipped,
		now,
	)
	return GetOpenRouterModelSyncState()
}

func normalizeOpenRouterModelSyncInterval(minutes int) int {
	if minutes <= 0 {
		return defaultOpenRouterModelSyncIntervalMinutes
	}
	if minutes < minOpenRouterModelSyncIntervalMinutes {
		return minOpenRouterModelSyncIntervalMinutes
	}
	return minutes
}

func isOpenRouterModelSyncDue(state OpenRouterModelSyncState, now time.Time) bool {
	if state.LastSyncAt == "" {
		return true
	}
	lastSync, err := time.Parse(time.RFC3339, state.LastSyncAt)
	if err != nil {
		return true
	}
	return now.Sub(lastSync) >= time.Duration(normalizeOpenRouterModelSyncInterval(state.IntervalMinutes))*time.Minute
}

func openRouterOwnerFromModelID(modelID string) string {
	prefix, _, found := strings.Cut(strings.TrimSpace(modelID), "/")
	if !found {
		return openRouterModelSource
	}
	prefix = strings.TrimLeft(prefix, "~～")
	if strings.TrimSpace(prefix) == "" {
		return openRouterModelSource
	}
	return normalizeModelOwnerValue(prefix)
}

func openRouterLocalModelID(remoteModelID, owner string) string {
	modelID := openRouterProviderlessModelID(remoteModelID)
	if normalizeModelOwnerValue(owner) == "anthropic" {
		modelID = strings.ReplaceAll(modelID, ".", "-")
		modelID = openRouterStripAnthropicReleaseDate(modelID)
	}
	return modelID
}

func openRouterProviderlessModelID(remoteModelID string) string {
	modelID := strings.TrimSpace(remoteModelID)
	if _, suffix, found := strings.Cut(modelID, "/"); found {
		return strings.TrimSpace(suffix)
	}
	return modelID
}

func openRouterStripAnthropicReleaseDate(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if !strings.HasPrefix(modelID, "claude-") || len(modelID) <= 9 {
		return modelID
	}
	dateStart := len(modelID) - 8
	if modelID[dateStart-1] != '-' {
		return modelID
	}
	for _, ch := range modelID[dateStart:] {
		if ch < '0' || ch > '9' {
			return modelID
		}
	}
	return modelID[:dateStart-1]
}

func openRouterLegacyLocalModelIDs(remoteModelID, owner, modelID string) []string {
	var ids []string
	seen := map[string]struct{}{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || id == modelID {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	add(remoteModelID)
	providerless := openRouterProviderlessModelID(remoteModelID)
	add(providerless)
	if normalizeModelOwnerValue(owner) == "anthropic" {
		add(strings.ReplaceAll(providerless, ".", "-"))
		for _, aliasID := range openRouterExistingAnthropicReleaseDateAliasIDs(modelID) {
			add(aliasID)
		}
	}
	return ids
}

func openRouterExistingAnthropicReleaseDateAliasIDs(modelID string) []string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	prefix := modelID + "-"
	var aliases []string
	for _, row := range ListModelConfigs() {
		aliasID := strings.TrimSpace(row.ModelID)
		if aliasID == modelID || !strings.HasPrefix(aliasID, prefix) {
			continue
		}
		if openRouterStripAnthropicReleaseDate(aliasID) == modelID {
			aliases = append(aliases, aliasID)
		}
	}
	return aliases
}

func openRouterApplyModelSync(row *ModelConfigRow, model OpenRouterRemoteModel, owner string) {
	if row == nil {
		return
	}
	if row.OwnedBy == "" || ownerMatchesOpenRouterAliasPrefix(row.OwnedBy, owner) {
		row.OwnedBy = owner
	}
	if description := openRouterModelDescription(model); description != "" && openRouterShouldSyncDescription(*row) {
		row.Description = description
	}
	row.PricingMode = "token"
	row.InputPricePerMillion = openRouterPricePerMillion(model.Pricing.Prompt)
	row.OutputPricePerMillion = openRouterPricePerMillion(model.Pricing.Completion)
	row.CachedPricePerMillion = openRouterPricePerMillion(model.Pricing.InputCacheRead)
	row.PricePerCall = 0
}

func openRouterShouldSyncDescription(row ModelConfigRow) bool {
	if strings.TrimSpace(row.Description) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(row.Source)) {
	case openRouterModelSource, "seed":
		return true
	default:
		return false
	}
}

func openRouterMigrateLegacyOpenRouterRow(modelID, owner string, model OpenRouterRemoteModel, legacyModelIDs []string) (bool, error) {
	for _, legacyModelID := range legacyModelIDs {
		if openRouterIsAnthropicReleaseDateAlias(legacyModelID, modelID) {
			continue
		}
		existing, exists := GetModelConfig(legacyModelID)
		if !exists || existing.Source != openRouterModelSource {
			continue
		}
		existing.ModelID = modelID
		openRouterApplyModelSync(&existing, model, owner)
		if err := UpsertModelConfig(existing); err != nil {
			return false, fmt.Errorf("migrate openrouter model %s to %s: %w", legacyModelID, modelID, err)
		}
		if err := openRouterDeleteLegacyOpenRouterRows(modelID, legacyModelIDs); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func openRouterDeleteLegacyOpenRouterRows(baseModelID string, modelIDs []string) error {
	for _, modelID := range modelIDs {
		if openRouterIsAnthropicReleaseDateAlias(modelID, baseModelID) {
			continue
		}
		existing, exists := GetModelConfig(modelID)
		if !exists || existing.Source != openRouterModelSource {
			continue
		}
		if err := DeleteModelConfig(modelID); err != nil {
			return fmt.Errorf("delete old openrouter model %s: %w", modelID, err)
		}
	}
	return nil
}

func openRouterSyncExistingAliasRows(baseModelID string, model OpenRouterRemoteModel, owner string, modelIDs []string) error {
	for _, modelID := range modelIDs {
		existing, exists := GetModelConfig(modelID)
		if !exists {
			continue
		}
		if existing.Source == openRouterModelSource && !openRouterIsAnthropicReleaseDateAlias(modelID, baseModelID) {
			continue
		}
		openRouterApplyModelSync(&existing, model, owner)
		if err := UpsertModelConfig(existing); err != nil {
			return fmt.Errorf("sync openrouter model alias %s: %w", modelID, err)
		}
	}
	return nil
}

func openRouterIsAnthropicReleaseDateAlias(modelID, baseModelID string) bool {
	modelID = strings.TrimSpace(modelID)
	baseModelID = strings.TrimSpace(baseModelID)
	return modelID != "" && baseModelID != "" && modelID != baseModelID && openRouterStripAnthropicReleaseDate(modelID) == baseModelID
}

func ownerMatchesOpenRouterAliasPrefix(owner, cleanOwner string) bool {
	owner = normalizeModelOwnerValue(owner)
	cleanOwner = normalizeModelOwnerValue(cleanOwner)
	if owner == "" || cleanOwner == "" {
		return false
	}
	return (strings.HasPrefix(owner, "~") || strings.HasPrefix(owner, "～")) && strings.TrimLeft(owner, "~～") == cleanOwner
}

func openRouterModelDescription(model OpenRouterRemoteModel) string {
	if description := strings.TrimSpace(model.Description); description != "" {
		return description
	}
	return strings.TrimSpace(model.Name)
}

func openRouterPricePerMillion(value string) float64 {
	price, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || price <= 0 {
		return 0
	}
	return math.Round(price*1_000_000*1_000_000_000_000) / 1_000_000_000_000
}
