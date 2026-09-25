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
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

const openRouterModelSource = "openrouter"
const defaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models?output_modalities=all"
const defaultOpenRouterModelSyncIntervalMinutes = 24 * 60
const minOpenRouterModelSyncIntervalMinutes = 60

type OpenRouterRemotePricing struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read"`
	InputCacheWrite string `json:"input_cache_write"`
}

type OpenRouterRemoteArchitecture struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

type OpenRouterRemoteTopProvider struct {
	ContextLength       int  `json:"context_length"`
	MaxCompletionTokens int  `json:"max_completion_tokens"`
	IsModerated         bool `json:"is_moderated"`
}

type OpenRouterRemoteReasoning struct {
	Mandatory        bool     `json:"mandatory"`
	DefaultEnabled   bool     `json:"default_enabled"`
	SupportedEfforts []string `json:"supported_efforts"`
	DefaultEffort    string   `json:"default_effort"`
	Exclude          bool     `json:"exclude"`
}

type OpenRouterRemoteModel struct {
	ID                  string                       `json:"id"`
	Name                string                       `json:"name"`
	Description         string                       `json:"description"`
	ContextLength       int                          `json:"context_length"`
	KnowledgeCutoff     string                       `json:"knowledge_cutoff"`
	SupportedParameters []string                     `json:"supported_parameters"`
	Architecture        OpenRouterRemoteArchitecture `json:"architecture"`
	Pricing             OpenRouterRemotePricing      `json:"pricing"`
	TopProvider         OpenRouterRemoteTopProvider  `json:"top_provider"`
	Reasoning           *OpenRouterRemoteReasoning   `json:"reasoning"`
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

	openRouterSyncRunning       sync.Map
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
	return SyncOpenRouterModelListForTenant(ctx, systemTenantID, models)
}

func SyncOpenRouterModelListForTenant(ctx context.Context, tenantID string, models []OpenRouterRemoteModel) (OpenRouterModelSyncResult, error) {
	tenantID = normalizeTenantID(tenantID)
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
		legacyModelIDs := openRouterLegacyLocalModelIDs(tenantID, remoteModelID, owner, modelID)
		if existing, exists := GetModelConfigForTenant(tenantID, modelID); exists {
			openRouterApplyModelSync(&existing, model, owner)
			if err := UpsertModelConfigForTenant(tenantID, existing); err != nil {
				return result, fmt.Errorf("sync openrouter model pricing %s: %w", modelID, err)
			}
			if err := openRouterDeleteLegacyOpenRouterRows(tenantID, modelID, legacyModelIDs); err != nil {
				return result, err
			}
			if err := openRouterSyncExistingAliasRows(tenantID, modelID, model, owner, legacyModelIDs); err != nil {
				return result, err
			}
			if err := openRouterSyncExistingWrapperRows(tenantID, modelID, model); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}
		migrated, err := openRouterMigrateLegacyOpenRouterRow(tenantID, modelID, owner, model, legacyModelIDs)
		if err != nil {
			return result, err
		}
		if migrated {
			if err := openRouterSyncExistingAliasRows(tenantID, modelID, model, owner, legacyModelIDs); err != nil {
				return result, err
			}
			if err := openRouterSyncExistingWrapperRows(tenantID, modelID, model); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}

		row := ModelConfigRow{
			ModelID:     modelID,
			OwnedBy:     owner,
			Description: openRouterModelDescription(model),
			Enabled:     true,
			Source:      openRouterModelSource,
		}
		// Populate pricing, modalities, and OpenRouter metadata through the shared update path.
		openRouterApplyModelSyncValues(&row, model)
		if err := UpsertModelConfigForTenant(tenantID, row); err != nil {
			return result, fmt.Errorf("sync openrouter model %s: %w", modelID, err)
		}
		if err := openRouterSyncExistingAliasRows(tenantID, modelID, model, owner, legacyModelIDs); err != nil {
			return result, err
		}
		if err := openRouterSyncExistingWrapperRows(tenantID, modelID, model); err != nil {
			return result, err
		}
		result.Added++
	}
	if err := openRouterMergeVariantGroups(tenantID, models); err != nil {
		return result, err
	}
	return result, nil
}

func GetOpenRouterModelSyncState() OpenRouterModelSyncState {
	return GetOpenRouterModelSyncStateForTenant(systemTenantID)
}

func GetOpenRouterModelSyncStateForTenant(tenantID string) OpenRouterModelSyncState {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	state := OpenRouterModelSyncState{
		IntervalMinutes: defaultOpenRouterModelSyncIntervalMinutes,
		Running:         openRouterModelSyncRunning(tenantID),
	}
	if db == nil {
		return state
	}
	ensureOpenRouterModelSyncStateRowForTenant(tenantID)
	var enabled int
	if err := db.QueryRow(
		`SELECT enabled, interval_minutes, last_sync_at, last_success_at, last_error, last_seen, last_added, last_updated, last_skipped, updated_at
		 FROM model_openrouter_sync_state WHERE tenant_id = ? AND id = 1`,
		tenantID,
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
	state.Running = openRouterModelSyncRunning(tenantID)
	return state
}

func UpdateOpenRouterModelSyncSettings(enabled bool, intervalMinutes int) (OpenRouterModelSyncState, error) {
	return UpdateOpenRouterModelSyncSettingsForTenant(systemTenantID, enabled, intervalMinutes)
}

func UpdateOpenRouterModelSyncSettingsForTenant(tenantID string, enabled bool, intervalMinutes int) (OpenRouterModelSyncState, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return OpenRouterModelSyncState{}, fmt.Errorf("usage: database not initialised")
	}
	ensureOpenRouterModelSyncStateRowForTenant(tenantID)
	_, err := db.Exec(
		`UPDATE model_openrouter_sync_state
		 SET enabled = ?, interval_minutes = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = 1`,
		boolToInt(enabled),
		normalizeOpenRouterModelSyncInterval(intervalMinutes),
		nowRFC3339(),
		tenantID,
	)
	if err != nil {
		return OpenRouterModelSyncState{}, fmt.Errorf("usage: update openrouter sync settings: %w", err)
	}
	return GetOpenRouterModelSyncStateForTenant(tenantID), nil
}

func RunOpenRouterModelSync(ctx context.Context) (OpenRouterModelSyncResult, OpenRouterModelSyncState, error) {
	return RunOpenRouterModelSyncForTenant(ctx, systemTenantID)
}

func RunOpenRouterModelSyncForTenant(ctx context.Context, tenantID string) (OpenRouterModelSyncResult, OpenRouterModelSyncState, error) {
	tenantID = normalizeTenantID(tenantID)
	if ctx == nil {
		ctx = context.Background()
	}
	if _, loaded := openRouterSyncRunning.LoadOrStore(tenantID, struct{}{}); loaded {
		state := GetOpenRouterModelSyncStateForTenant(tenantID)
		return OpenRouterModelSyncResult{}, state, fmt.Errorf("usage: openrouter model sync already running")
	}
	defer openRouterSyncRunning.Delete(tenantID)

	openRouterModelFetcherMu.RLock()
	fetcher := openRouterModelFetcher
	openRouterModelFetcherMu.RUnlock()
	if fetcher == nil {
		fetcher = fetchOpenRouterModels
	}

	models, err := fetcher(ctx)
	if err != nil {
		state := recordOpenRouterModelSyncResultForTenant(tenantID, OpenRouterModelSyncResult{}, err)
		return OpenRouterModelSyncResult{}, state, err
	}
	result, err := SyncOpenRouterModelListForTenant(ctx, tenantID, models)
	state := recordOpenRouterModelSyncResultForTenant(tenantID, result, err)
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

	runIfDue := func() { runDueOpenRouterModelSyncs(ctx) }

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

func openRouterModelSyncRunning(tenantID string) bool {
	_, running := openRouterSyncRunning.Load(normalizeTenantID(tenantID))
	return running
}

func enabledOpenRouterSyncTenantIDs() []string {
	db := getDB()
	if db == nil {
		return nil
	}
	ensureOpenRouterModelSyncStateRowForTenant(systemTenantID)
	rows, err := db.Query(`SELECT tenant_id FROM model_openrouter_sync_state WHERE enabled = 1 ORDER BY tenant_id`)
	if err != nil {
		log.Warnf("usage: list enabled openrouter model sync tenants: %v", err)
		return nil
	}
	defer rows.Close()
	var tenantIDs []string
	for rows.Next() {
		var tenantID string
		if rows.Scan(&tenantID) == nil {
			tenantIDs = append(tenantIDs, normalizeTenantID(tenantID))
		}
	}
	return tenantIDs
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

func ensureOpenRouterModelSyncStateRowForTenant(tenantID string) {
	db := getDB()
	if db == nil {
		return
	}
	ensureOpenRouterModelSyncStateSchema(db)
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO model_openrouter_sync_state
		 (tenant_id, id, enabled, interval_minutes, last_sync_at, last_success_at, last_error, last_seen, last_added, last_updated, last_skipped, updated_at)
		 VALUES (?, 1, 0, ?, '', '', '', 0, 0, 0, 0, ?)`,
		tenantID,
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

func recordOpenRouterModelSyncResultForTenant(tenantID string, result OpenRouterModelSyncResult, syncErr error) OpenRouterModelSyncState {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return GetOpenRouterModelSyncStateForTenant(tenantID)
	}
	ensureOpenRouterModelSyncStateRowForTenant(tenantID)
	now := nowRFC3339()
	state := GetOpenRouterModelSyncStateForTenant(tenantID)
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
		 WHERE tenant_id = ? AND id = 1`,
		now,
		lastSuccessAt,
		lastError,
		result.Seen,
		result.Added,
		result.Updated,
		result.Skipped,
		now,
		tenantID,
	)
	return GetOpenRouterModelSyncStateForTenant(tenantID)
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
	openRouterApplyModelSyncValues(row, model)
}

func openRouterApplyModelSyncValues(row *ModelConfigRow, model OpenRouterRemoteModel) {
	if row == nil {
		return
	}
	if openRouterApplyImageGenerationSemantics(row, model) {
		openRouterApplyModelMetadata(row, model)
		return
	}
	row.PricingMode = "token"
	row.InputPricePerMillion = openRouterPricePerMillion(model.Pricing.Prompt)
	row.OutputPricePerMillion = openRouterPricePerMillion(model.Pricing.Completion)
	row.CachedPricePerMillion = openRouterPricePerMillion(model.Pricing.InputCacheRead)
	if cacheWrite := openRouterPricePerMillion(model.Pricing.InputCacheWrite); cacheWrite > 0 {
		row.CacheWritePricePerMillion = cacheWrite
	}
	// Keep cache-read aligned with OpenRouter when available.
	if cacheRead := openRouterPricePerMillion(model.Pricing.InputCacheRead); cacheRead > 0 {
		row.CacheReadPricePerMillion = cacheRead
	}
	row.PricePerCall = 0
	inputModalities, outputModalities := openRouterModelModalities(model)
	if len(inputModalities) > 0 {
		row.InputModalities = inputModalities
	}
	if len(outputModalities) > 0 {
		row.OutputModalities = outputModalities
	}
	openRouterApplyModelMetadata(row, model)
}

func openRouterApplyModelMetadata(row *ModelConfigRow, model OpenRouterRemoteModel) {
	if row == nil {
		return
	}
	if displayName := openRouterModelDisplayName(model); displayName != "" && openRouterShouldSyncDisplayName(*row) {
		row.DisplayName = displayName
	}
	if contextLength := openRouterModelContextLength(model); contextLength > 0 {
		row.ContextLength = contextLength
	}
	if maxCompletion := openRouterModelMaxCompletionTokens(model); maxCompletion > 0 {
		row.MaxCompletionTokens = maxCompletion
	}
	if params := openRouterModelSupportedParameters(model); len(params) > 0 {
		row.SupportedParameters = params
	}
	if reasoning := openRouterModelReasoningJSON(model); reasoning != "" {
		row.Reasoning = reasoning
	}
	if cutoff := strings.TrimSpace(model.KnowledgeCutoff); cutoff != "" {
		row.KnowledgeCutoff = cutoff
	}
}

func openRouterShouldSyncDisplayName(row ModelConfigRow) bool {
	if strings.TrimSpace(row.DisplayName) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(row.Source)) {
	case openRouterModelSource, "seed":
		return true
	default:
		return false
	}
}

func openRouterModelDisplayName(model OpenRouterRemoteModel) string {
	name := strings.TrimSpace(model.Name)
	if name == "" {
		return ""
	}
	// OpenRouter names often look like "OpenAI: GPT-5". Prefer the model label.
	if _, right, found := strings.Cut(name, ":"); found {
		if trimmed := strings.TrimSpace(right); trimmed != "" {
			return trimmed
		}
	}
	return name
}

func openRouterModelContextLength(model OpenRouterRemoteModel) int {
	if model.ContextLength > 0 {
		return model.ContextLength
	}
	if model.TopProvider.ContextLength > 0 {
		return model.TopProvider.ContextLength
	}
	return 0
}

func openRouterModelMaxCompletionTokens(model OpenRouterRemoteModel) int {
	if model.TopProvider.MaxCompletionTokens > 0 {
		return model.TopProvider.MaxCompletionTokens
	}
	return 0
}

func openRouterModelSupportedParameters(model OpenRouterRemoteModel) []string {
	return normalizeModelModalities(model.SupportedParameters)
}

func openRouterModelReasoningJSON(model OpenRouterRemoteModel) string {
	if model.Reasoning == nil {
		return ""
	}
	payload := map[string]any{
		"mandatory":         model.Reasoning.Mandatory,
		"default_enabled":   model.Reasoning.DefaultEnabled,
		"supported_efforts": normalizeModelModalities(model.Reasoning.SupportedEfforts),
		"default_effort":    strings.TrimSpace(model.Reasoning.DefaultEffort),
		"exclude":           model.Reasoning.Exclude,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func openRouterApplyImageGenerationSemantics(row *ModelConfigRow, model OpenRouterRemoteModel) bool {
	if row == nil || !openRouterIsImageGenerationRow(*row) {
		return false
	}
	row.PricingMode = "call"
	row.InputPricePerMillion = 0
	row.OutputPricePerMillion = 0
	row.CachedPricePerMillion = 0
	if row.PricePerCall <= 0 {
		row.PricePerCall = openRouterDefaultImageGenerationPricePerCall(row.ModelID)
	}

	inputModalities, outputModalities := openRouterModelModalities(model)
	row.InputModalities = unionModalities(row.InputModalities, inputModalities)
	row.OutputModalities = unionModalities(
		imageOutputModalities(row.OutputModalities),
		imageOutputModalities(outputModalities),
	)
	if len(row.InputModalities) == 0 {
		row.InputModalities = []string{"text"}
	}
	row.OutputModalities = unionModalities(row.OutputModalities, []string{"image"})
	return true
}

func openRouterIsImageGenerationRow(row ModelConfigRow) bool {
	modelID := strings.ToLower(strings.TrimSpace(row.ModelID))
	if registry.IsImageGenerationModel(modelID) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(row.PricingMode), "call") {
		return true
	}
	for _, modality := range row.OutputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

func openRouterDefaultImageGenerationPricePerCall(modelID string) float64 {
	if price, _, ok := registry.ImageGenerationModelDefaults(modelID); ok {
		return price
	}
	return 0
}

func imageOutputModalities(modalities []string) []string {
	for _, modality := range modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return []string{"image"}
		}
	}
	return nil
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

func openRouterMigrateLegacyOpenRouterRow(tenantID, modelID, owner string, model OpenRouterRemoteModel, legacyModelIDs []string) (bool, error) {
	for _, legacyModelID := range legacyModelIDs {
		if openRouterIsDateSuffixAlias(legacyModelID, modelID) {
			continue
		}
		existing, exists := GetModelConfigForTenant(tenantID, legacyModelID)
		if !exists || existing.Source != openRouterModelSource {
			continue
		}
		existing.ModelID = modelID
		openRouterApplyModelSync(&existing, model, owner)
		if err := UpsertModelConfigForTenant(tenantID, existing); err != nil {
			return false, fmt.Errorf("migrate openrouter model %s to %s: %w", legacyModelID, modelID, err)
		}
		if err := openRouterDeleteLegacyOpenRouterRows(tenantID, modelID, legacyModelIDs); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func openRouterDeleteLegacyOpenRouterRows(tenantID, baseModelID string, modelIDs []string) error {
	for _, modelID := range modelIDs {
		if openRouterIsDateSuffixAlias(modelID, baseModelID) {
			continue
		}
		existing, exists := GetModelConfigForTenant(tenantID, modelID)
		if !exists || existing.Source != openRouterModelSource {
			continue
		}
		if err := DeleteModelConfigForTenant(tenantID, modelID); err != nil {
			return fmt.Errorf("delete old openrouter model %s: %w", modelID, err)
		}
	}
	return nil
}

func openRouterSyncExistingAliasRows(tenantID, baseModelID string, model OpenRouterRemoteModel, owner string, modelIDs []string) error {
	for _, modelID := range modelIDs {
		existing, exists := GetModelConfigForTenant(tenantID, modelID)
		if !exists {
			continue
		}
		if existing.Source == openRouterModelSource && !openRouterIsDateSuffixAlias(modelID, baseModelID) {
			continue
		}
		openRouterApplyModelSync(&existing, model, owner)
		if err := UpsertModelConfigForTenant(tenantID, existing); err != nil {
			return fmt.Errorf("sync openrouter model alias %s: %w", modelID, err)
		}
	}
	return nil
}

func openRouterSyncExistingWrapperRows(tenantID, baseModelID string, model OpenRouterRemoteModel) error {
	for _, wrapperID := range openRouterWrapperModelIDs(baseModelID) {
		existing, exists := GetModelConfigForTenant(tenantID, wrapperID)
		if !exists {
			continue
		}
		switch {
		case strings.HasPrefix(wrapperID, "cline-pass/"):
			existing.OwnedBy = "cline"
		case strings.HasPrefix(wrapperID, "ollama/"):
			existing.OwnedBy = "ollama"
		}
		if description := openRouterModelDescription(model); description != "" && openRouterShouldSyncDescription(existing) {
			existing.Description = description
		}
		openRouterApplyModelSyncValues(&existing, model)
		if err := UpsertModelConfigForTenant(tenantID, existing); err != nil {
			return fmt.Errorf("sync openrouter model wrapper %s: %w", wrapperID, err)
		}
	}
	return nil
}

func openRouterWrapperModelIDs(baseModelID string) []string {
	baseModelID = strings.TrimSpace(baseModelID)
	if baseModelID == "" || strings.Contains(baseModelID, "/") {
		return nil
	}
	return []string{
		"cline-pass/" + baseModelID,
		"ollama/" + baseModelID,
	}
}

func openRouterStaticBaseModelRow(modelID string) (ModelConfigRow, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ModelConfigRow{}, false
	}
	info := registry.LookupStaticModelInfo(modelID)
	if info == nil {
		return ModelConfigRow{}, false
	}
	ownedBy := normalizeModelOwnerValue(info.OwnedBy)
	if ownedBy == "" {
		ownedBy = normalizeModelOwnerValue(info.Type)
	}
	description := strings.TrimSpace(info.Description)
	if description == "" {
		description = strings.TrimSpace(info.DisplayName)
	}
	return ModelConfigRow{
		ModelID:             modelID,
		OwnedBy:             ownedBy,
		DisplayName:         strings.TrimSpace(info.DisplayName),
		Description:         description,
		Enabled:             true,
		ContextLength:       info.ContextLength,
		MaxCompletionTokens: info.MaxCompletionTokens,
		SupportedParameters: append([]string(nil), info.SupportedParameters...),
		PricingMode:         "token",
		Source:              "seed",
	}, true
}

// openRouterMergeVariantGroups performs a second pass after the main sync loop.
// It groups all remote models by their date-stripped base ID and updates existing
// base model config rows with the highest prices and best metadata found across
// all models that share the same base. This ensures:
//   - When OpenRouter only returns dated/versioned variants
//     (e.g. qwen3.5-plus-20260420), the base model (qwen3.5-plus) gets correct data.
//   - When an exact base match AND dated variants exist in the same sync batch,
//     the base model ends up with the highest prices from any member of the group,
//     rather than being overwritten by the last-processed variant.
func openRouterMergeVariantGroups(tenantID string, models []OpenRouterRemoteModel) error {
	type groupEntry struct {
		providerlessID string
		model          OpenRouterRemoteModel
	}
	groups := make(map[string][]groupEntry)

	for _, m := range models {
		remoteModelID := strings.TrimSpace(m.ID)
		if remoteModelID == "" {
			continue
		}
		providerless := openRouterProviderlessModelID(remoteModelID)
		baseID := openRouterCanonicalGroupID(remoteModelID)
		groups[baseID] = append(groups[baseID], groupEntry{
			providerlessID: providerless,
			model:          m,
		})
	}

	for baseID, entries := range groups {
		baseModel, exists := GetModelConfigForTenant(tenantID, baseID)
		if !exists {
			var ok bool
			baseModel, ok = openRouterStaticBaseModelRow(baseID)
			if !ok {
				continue
			}
		}
		imageGenerationBase := openRouterIsImageGenerationRow(baseModel)
		// Aggregate: highest prices, best description, most complete modalities/metadata.
		bestInputPrice := baseModel.InputPricePerMillion
		bestOutputPrice := baseModel.OutputPricePerMillion
		bestCachedPrice := baseModel.CachedPricePerMillion
		bestCacheWritePrice := baseModel.CacheWritePricePerMillion
		bestCacheReadPrice := baseModel.CacheReadPricePerMillion
		bestModalities := struct {
			input  []string
			output []string
		}{baseModel.InputModalities, baseModel.OutputModalities}
		bestDesc := ""
		bestDisplayName := ""
		bestContextLength := baseModel.ContextLength
		bestMaxCompletion := baseModel.MaxCompletionTokens
		bestParams := append([]string(nil), baseModel.SupportedParameters...)
		bestReasoning := baseModel.Reasoning
		bestKnowledgeCutoff := baseModel.KnowledgeCutoff

		for _, e := range entries {
			price := openRouterPricePerMillion(e.model.Pricing.Prompt)
			if price > bestInputPrice {
				bestInputPrice = price
			}
			price = openRouterPricePerMillion(e.model.Pricing.Completion)
			if price > bestOutputPrice {
				bestOutputPrice = price
			}
			price = openRouterPricePerMillion(e.model.Pricing.InputCacheRead)
			if price > bestCachedPrice {
				bestCachedPrice = price
			}
			if price > bestCacheReadPrice {
				bestCacheReadPrice = price
			}
			price = openRouterPricePerMillion(e.model.Pricing.InputCacheWrite)
			if price > bestCacheWritePrice {
				bestCacheWritePrice = price
			}
			if desc := openRouterModelDescription(e.model); desc != "" && (bestDesc == "" || len(desc) > len(bestDesc)) {
				bestDesc = desc
			}
			if displayName := openRouterModelDisplayName(e.model); displayName != "" && (bestDisplayName == "" || len(displayName) > len(bestDisplayName)) {
				bestDisplayName = displayName
			}
			if contextLength := openRouterModelContextLength(e.model); contextLength > bestContextLength {
				bestContextLength = contextLength
			}
			if maxCompletion := openRouterModelMaxCompletionTokens(e.model); maxCompletion > bestMaxCompletion {
				bestMaxCompletion = maxCompletion
			}
			bestParams = unionModalities(bestParams, openRouterModelSupportedParameters(e.model))
			if reasoning := openRouterModelReasoningJSON(e.model); reasoning != "" && (bestReasoning == "" || len(reasoning) > len(bestReasoning)) {
				bestReasoning = reasoning
			}
			if cutoff := strings.TrimSpace(e.model.KnowledgeCutoff); cutoff != "" {
				bestKnowledgeCutoff = cutoff
			}
			inMod, outMod := openRouterModelModalities(e.model)
			bestModalities.input = unionModalities(bestModalities.input, inMod)
			if imageGenerationBase {
				bestModalities.output = unionModalities(bestModalities.output, imageOutputModalities(outMod))
			} else {
				bestModalities.output = unionModalities(bestModalities.output, outMod)
			}
		}

		// Apply the merged data back to the base model.
		updated := false

		if !imageGenerationBase && (bestInputPrice != baseModel.InputPricePerMillion ||
			bestOutputPrice != baseModel.OutputPricePerMillion ||
			bestCachedPrice != baseModel.CachedPricePerMillion ||
			bestCacheReadPrice != baseModel.CacheReadPricePerMillion ||
			bestCacheWritePrice != baseModel.CacheWritePricePerMillion) {
			baseModel.PricingMode = "token"
			baseModel.InputPricePerMillion = bestInputPrice
			baseModel.OutputPricePerMillion = bestOutputPrice
			baseModel.CachedPricePerMillion = bestCachedPrice
			baseModel.CacheReadPricePerMillion = bestCacheReadPrice
			baseModel.CacheWritePricePerMillion = bestCacheWritePrice
			updated = true
		}

		if len(bestModalities.input) > len(baseModel.InputModalities) ||
			len(bestModalities.output) > len(baseModel.OutputModalities) {
			baseModel.InputModalities = bestModalities.input
			baseModel.OutputModalities = bestModalities.output
			updated = true
		}

		if bestDesc != "" && openRouterShouldSyncDescription(baseModel) {
			baseModel.Description = bestDesc
			updated = true
		}
		if bestDisplayName != "" && openRouterShouldSyncDisplayName(baseModel) {
			baseModel.DisplayName = bestDisplayName
			updated = true
		}
		if bestContextLength > baseModel.ContextLength {
			baseModel.ContextLength = bestContextLength
			updated = true
		}
		if bestMaxCompletion > baseModel.MaxCompletionTokens {
			baseModel.MaxCompletionTokens = bestMaxCompletion
			updated = true
		}
		if len(bestParams) > len(baseModel.SupportedParameters) {
			baseModel.SupportedParameters = bestParams
			updated = true
		}
		if bestReasoning != "" && (baseModel.Reasoning == "" || len(bestReasoning) > len(baseModel.Reasoning)) {
			baseModel.Reasoning = bestReasoning
			updated = true
		}
		if bestKnowledgeCutoff != "" && bestKnowledgeCutoff != baseModel.KnowledgeCutoff {
			baseModel.KnowledgeCutoff = bestKnowledgeCutoff
			updated = true
		}

		if updated {
			if err := UpsertModelConfigForTenant(tenantID, baseModel); err != nil {
				return fmt.Errorf("merge variant group for %s: %w", baseID, err)
			}
		}
	}

	return nil
}

// unionModalities returns a deduplicated union of two modality slices.
// The order is preserved from a, with any missing entries from b appended.
func unionModalities(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, m := range a {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	for _, m := range b {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
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

func openRouterModelModalities(model OpenRouterRemoteModel) ([]string, []string) {
	input := normalizeModelModalities(model.Architecture.InputModalities)
	output := normalizeModelModalities(model.Architecture.OutputModalities)
	if len(input) > 0 && len(output) > 0 {
		return input, output
	}

	modality := strings.TrimSpace(model.Architecture.Modality)
	if modality == "" {
		return input, output
	}
	left, right, found := strings.Cut(modality, "->")
	if !found {
		return input, output
	}
	if len(input) == 0 {
		input = parseOpenRouterModalityPart(left)
	}
	if len(output) == 0 {
		output = parseOpenRouterModalityPart(right)
	}
	return input, output
}

func parseOpenRouterModalityPart(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '+' || r == ',' || r == '|' || r == '/'
	})
	return normalizeModelModalities(parts)
}

func openRouterPricePerMillion(value string) float64 {
	price, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || price <= 0 {
		return 0
	}
	return math.Round(price*1_000_000*1_000_000_000_000) / 1_000_000_000_000
}
