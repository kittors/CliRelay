package apikey

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

var (
	ErrInvalidProfileID            = errors.New("id is required")
	ErrInvalidProfileName          = errors.New("name is required")
	ErrMissingValue                = errors.New("missing value")
	ErrInvalidEntry                = errors.New("invalid api key entry")
	ErrItemNotFound                = errors.New("item not found")
	ErrDuplicateKey                = errors.New("api key already exists")
	ErrMissingKeyOrIndex           = errors.New("missing key or index")
	ErrKeyRequired                 = errors.New("key is required")
	ErrDailySpendingLimitMissing   = errors.New("daily spending limit is not set")
	ErrOwnedKeyAccountManagedField = errors.New("owned key account-managed field cannot be changed")
)

type ChannelSanitizer func([]string) ([]string, error)
type ChannelGroupValidator func([]string) ([]string, error)
type EntryValidator func(config.APIKeyEntry) error
type LogsDeleter func(string) (int64, error)
type Option func(*Service)

type Service struct {
	tenantID             string
	sanitizeChannels     ChannelSanitizer
	validateChannelGroup ChannelGroupValidator
	validateEntry        EntryValidator
	deleteLogs           LogsDeleter
}

type EntryPatch struct {
	Key                  *string                          `json:"key"`
	Name                 *string                          `json:"name"`
	Disabled             *bool                            `json:"disabled"`
	PermissionProfileID  *string                          `json:"permission-profile-id"`
	DailyLimit           *int                             `json:"daily-limit"`
	TotalQuota           *int                             `json:"total-quota"`
	SpendingLimit        *float64                         `json:"spending-limit"`
	DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
	PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	ConcurrencyLimit     *int                             `json:"concurrency-limit"`
	RPMLimit             *int                             `json:"rpm-limit"`
	TPMLimit             *int                             `json:"tpm-limit"`
	AllowedModels        *[]string                        `json:"allowed-models"`
	AllowedChannels      *[]string                        `json:"allowed-channels"`
	AllowedChannelGroups *[]string                        `json:"allowed-channel-groups"`
	SystemPrompt         *string                          `json:"system-prompt"`
	CreatedAt            *string                          `json:"created-at"`
	// end_user_id / is_default are intentionally not patchable here; ownership
	// changes go through end-user owner-scoped APIs only.
}

type DeleteEntryResult struct {
	LogsDeleted int64
}

func WithTenantID(tenantID string) Option {
	return func(s *Service) {
		s.tenantID = strings.TrimSpace(tenantID)
	}
}

func WithChannelGroupValidator(fn ChannelGroupValidator) Option {
	return func(s *Service) {
		s.validateChannelGroup = fn
	}
}

func WithEntryValidator(fn EntryValidator) Option {
	return func(s *Service) {
		s.validateEntry = fn
	}
}

func WithLogsDeleter(fn LogsDeleter) Option {
	return func(s *Service) {
		s.deleteLogs = fn
	}
}

func NewService(sanitizeChannels ChannelSanitizer, opts ...Option) *Service {
	svc := &Service{sanitizeChannels: sanitizeChannels}
	for _, opt := range opts {
		if opt != nil {
			opt(svc)
		}
	}
	return svc
}

func (s *Service) EnabledKeys() []string {
	rows := usage.ListAPIKeysForTenant(s.tenantID)
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if !row.Disabled {
			keys = append(keys, row.Key)
		}
	}
	return keys
}

func (s *Service) ListRows() []usage.APIKeyRow {
	return usage.ListAPIKeysForTenant(s.tenantID)
}

func (s *Service) GetRow(key string) *usage.APIKeyRow {
	return usage.GetAPIKeyForTenant(s.tenantID, strings.TrimSpace(key))
}

func (s *Service) GetRowByID(id string) *usage.APIKeyRow {
	return usage.GetAPIKeyByIDForTenant(s.tenantID, strings.TrimSpace(id))
}

func (s *Service) ReplaceKeys(keys []string) error {
	rows := make([]usage.APIKeyRow, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			rows = append(rows, usage.APIKeyRow{Key: trimmed})
		}
	}
	return usage.ReplaceAllAPIKeysForTenant(s.tenantID, rows)
}

func (s *Service) PatchKey(oldKey string, newKey string) error {
	oldKey = strings.TrimSpace(oldKey)
	newKey = strings.TrimSpace(newKey)
	if oldKey == "" {
		if newKey == "" {
			return nil
		}
		return usage.UpsertAPIKeyForTenant(s.tenantID, usage.APIKeyRow{Key: newKey})
	}
	existing := usage.GetAPIKeyForTenant(s.tenantID, oldKey)
	if existing == nil {
		if newKey == "" {
			return nil
		}
		return usage.UpsertAPIKeyForTenant(s.tenantID, usage.APIKeyRow{Key: newKey})
	}
	if newKey == "" {
		return usage.DeleteAPIKeyByIDForTenant(s.tenantID, existing.ID)
	}
	updated := *existing
	updated.Key = newKey
	return usage.UpdateAPIKeyByIDForTenant(s.tenantID, updated)
}

func (s *Service) DeleteKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrMissingValue
	}
	return usage.DeleteAPIKeyForTenant(s.tenantID, key)
}

func (s *Service) PermissionProfiles() []usage.APIKeyPermissionProfileRow {
	return usage.ListAPIKeyPermissionProfilesForTenant(s.tenantID)
}

func (s *Service) ReplacePermissionProfiles(profiles []usage.APIKeyPermissionProfileRow) error {
	normalized, err := s.normalizePermissionProfiles(profiles)
	if err != nil {
		return err
	}
	return usage.ReplaceAllAPIKeyPermissionProfilesForTenant(s.tenantID, normalized)
}

func (s *Service) ReplacePermissionProfilesAndSyncAccounts(profiles []usage.APIKeyPermissionProfileRow) (int64, error) {
	result, err := s.ReplacePermissionProfilesWithCaps(profiles, true)
	return result.AppliedCount, err
}

func (s *Service) ReplacePermissionProfilesWithCaps(profiles []usage.APIKeyPermissionProfileRow, syncAccounts bool) (usage.APIKeyPermissionProfileSyncResult, error) {
	normalized, err := s.normalizePermissionProfiles(profiles)
	if err != nil {
		return usage.APIKeyPermissionProfileSyncResult{}, err
	}
	return usage.ReplaceAllAPIKeyPermissionProfilesForTenantWithCaps(s.tenantID, normalized, syncAccounts)
}

func (s *Service) normalizePermissionProfiles(profiles []usage.APIKeyPermissionProfileRow) ([]usage.APIKeyPermissionProfileRow, error) {
	normalized := make([]usage.APIKeyPermissionProfileRow, len(profiles))
	copy(normalized, profiles)
	for idx := range normalized {
		normalized[idx].ID = strings.TrimSpace(normalized[idx].ID)
		normalized[idx].Name = strings.TrimSpace(normalized[idx].Name)
		if normalized[idx].ID == "" {
			return nil, ErrInvalidProfileID
		}
		if normalized[idx].Name == "" {
			return nil, ErrInvalidProfileName
		}
		limits, err := quota.NormalizeLimits(normalized[idx].PeriodSpendingLimits)
		if err != nil {
			return nil, err
		}
		day, err := quota.NormalizeWholeUSD(normalized[idx].DailySpendingLimit)
		if err != nil {
			return nil, err
		}
		if limits.Day > 0 && day > 0 && limits.Day != day {
			return nil, quota.ErrPeriodDayLegacyConflict
		}
		if day == 0 {
			day = limits.Day
		}
		limits.Day = day
		normalized[idx].DailySpendingLimit = day
		normalized[idx].PeriodSpendingLimits = limits
		if s != nil && s.sanitizeChannels != nil {
			cleaned, err := s.sanitizeChannels(normalized[idx].AllowedChannels)
			if err != nil {
				return nil, err
			}
			normalized[idx].AllowedChannels = cleaned
		}
	}
	return normalized, nil
}

func (s *Service) RenameAllowedChannelRestrictions(oldNameSet map[string]struct{}, newName string) error {
	for _, row := range usage.ListAPIKeysForTenant(s.tenantID) {
		channels, changed := renameChannelRestrictions(row.AllowedChannels, oldNameSet, newName)
		if !changed {
			continue
		}
		row.AllowedChannels = channels
		if err := usage.UpsertAPIKeyForTenant(s.tenantID, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RemoveAllowedChannelRestrictions(oldNameSet map[string]struct{}) error {
	for _, row := range usage.ListAPIKeysForTenant(s.tenantID) {
		channels, changed := removeChannelRestrictions(row.AllowedChannels, oldNameSet)
		if !changed {
			continue
		}
		row.AllowedChannels = channels
		if err := usage.UpsertAPIKeyForTenant(s.tenantID, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RenamePermissionProfileChannelRestrictions(oldNameSet map[string]struct{}, newName string) error {
	profiles := usage.ListAPIKeyPermissionProfilesForTenant(s.tenantID)
	changed := false
	for idx := range profiles {
		channels, channelsChanged := renameChannelRestrictions(profiles[idx].AllowedChannels, oldNameSet, newName)
		if !channelsChanged {
			continue
		}
		profiles[idx].AllowedChannels = channels
		changed = true
	}
	if !changed {
		return nil
	}
	return s.ReplacePermissionProfiles(profiles)
}

func (s *Service) RemovePermissionProfileChannelRestrictions(oldNameSet map[string]struct{}) error {
	profiles := usage.ListAPIKeyPermissionProfilesForTenant(s.tenantID)
	changed := false
	for idx := range profiles {
		channels, channelsChanged := removeChannelRestrictions(profiles[idx].AllowedChannels, oldNameSet)
		if !channelsChanged {
			continue
		}
		profiles[idx].AllowedChannels = channels
		changed = true
	}
	if !changed {
		return nil
	}
	return s.ReplacePermissionProfiles(profiles)
}

func (s *Service) ListEntries() []config.APIKeyEntry {
	entries, _ := s.ListEntriesWithDailySpending()
	return entries
}

// ListEntriesWithDailySpending lists keys and attaches effective daily spending fields.
// Query failures return an error so management handlers do not silently show $0.
func (s *Service) ListEntriesWithDailySpending() ([]config.APIKeyEntry, error) {
	all := usage.ListAPIKeysForTenant(s.tenantID)
	// Soft-deleted owned keys keep a sk-deleted-* tombstone secret for log
	// ownership; hide them from management lists. Intentional disable (same
	// secret, disabled=1) still appears so operators can re-enable.
	visible := make([]usage.APIKeyRow, 0, len(all))
	for _, row := range all {
		if row.Disabled && strings.TrimSpace(row.EndUserID) != "" && strings.HasPrefix(strings.TrimSpace(row.Key), "sk-deleted-") {
			continue
		}
		visible = append(visible, row)
	}
	rows := usage.EffectiveAPIKeyRowsForTenant(s.tenantID, visible)
	entries := make([]config.APIKeyEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, row.ToConfigEntry())
	}
	if err := s.attachDailySpendingRuntime(entries, rows); err != nil {
		return nil, err
	}
	return entries, nil
}

// attachDailySpendingRuntime fills daily-spending-used / daily-spending-remaining via batch queries.
func (s *Service) attachDailySpendingRuntime(entries []config.APIKeyEntry, rows []usage.APIKeyRow) error {
	if len(entries) == 0 || len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if id := strings.TrimSpace(row.ID); id != "" {
			ids = append(ids, id)
		}
	}
	usedByID, err := usage.QueryPeriodSpendingByAPIKeyIDsForTenant(s.tenantID, ids)
	if err != nil {
		return err
	}
	counts, err := usage.ListDailySpendingResetEventCounts(s.tenantID, ids)
	if err != nil {
		return err
	}
	for i := range entries {
		id := strings.TrimSpace(rows[i].ID)
		used := usedByID[id]
		entries[i].PeriodSpendingLimits.Day = entries[i].DailySpendingLimit
		entries[i].DailySpendingUsed = used.Day
		entries[i].LifetimeSpendingUsed = used.Lifetime
		entries[i].PeriodSpending = quota.BuildPeriodSpending(entries[i].PeriodSpendingLimits, used)
		entries[i].DailySpendingRemaining = usage.DailySpendingRemaining(entries[i].DailySpendingLimit, used.Day)
		entries[i].DailySpendingResetCount = counts[id]
	}
	return nil
}

func (s *Service) ReplaceEntries(entries []config.APIKeyEntry) error {
	rows := make([]usage.APIKeyRow, 0, len(entries))
	for _, entry := range entries {
		existing := usage.GetAPIKeyByIDForTenant(s.tenantID, strings.TrimSpace(entry.ID))
		if existing == nil {
			existing = usage.GetAPIKeyForTenant(s.tenantID, strings.TrimSpace(entry.Key))
		}
		if existing != nil && strings.TrimSpace(existing.EndUserID) != "" {
			if entry.PermissionProfileID != "" || entry.DailyLimit != 0 || entry.TotalQuota != 0 || entry.SpendingLimit != 0 ||
				entry.ConcurrencyLimit != 0 || entry.RPMLimit != 0 || entry.TPMLimit != 0 || len(entry.AllowedModels) != 0 ||
				len(entry.AllowedChannels) != 0 || len(entry.AllowedChannelGroups) != 0 || entry.SystemPrompt != "" {
				return ErrOwnedKeyAccountManagedField
			}
			incoming := entry.PeriodSpendingLimits
			incoming.Day = entry.DailySpendingLimit
			if incoming != existing.PeriodSpendingLimits {
				return fmt.Errorf("%w: update owned key period limits with PATCH", ErrInvalidEntry)
			}
			entry.EndUserID = existing.EndUserID
			entry.IsDefault = existing.IsDefault
		}
		normalized, err := s.prepareEntryForSave(entry)
		if err != nil {
			return err
		}
		rows = append(rows, usage.APIKeyRowFromConfig(normalized))
	}
	return usage.ReplaceAllAPIKeysForTenant(s.tenantID, rows)
}

func (s *Service) PatchEntry(id *string, index *int, match *string, patch EntryPatch) error {
	existing := s.resolvePatchTargetRow(id, index, match)
	if existing == nil {
		return ErrItemNotFound
	}
	entry := *existing
	owned := strings.TrimSpace(existing.EndUserID) != ""
	if owned && (patch.PermissionProfileID != nil || patch.DailyLimit != nil || patch.TotalQuota != nil ||
		patch.SpendingLimit != nil || patch.ConcurrencyLimit != nil || patch.RPMLimit != nil || patch.TPMLimit != nil ||
		patch.AllowedModels != nil || patch.AllowedChannels != nil || patch.AllowedChannelGroups != nil || patch.SystemPrompt != nil) {
		return ErrOwnedKeyAccountManagedField
	}
	periodPatch, err := quota.ResolveLegacyDay(patch.DailySpendingLimit, patch.PeriodSpendingLimits)
	if err != nil {
		return err
	}
	originalKey := strings.TrimSpace(entry.Key)
	originalID := strings.TrimSpace(entry.ID)

	if patch.Key != nil {
		entry.Key = strings.TrimSpace(*patch.Key)
	}
	if patch.Name != nil {
		entry.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Disabled != nil {
		// Tombstoned owned secrets are permanently revoked; never re-enable them.
		if !*patch.Disabled && strings.HasPrefix(strings.TrimSpace(existing.Key), "sk-deleted-") {
			return ErrItemNotFound
		}
		entry.Disabled = *patch.Disabled
	}
	if patch.PermissionProfileID != nil {
		entry.PermissionProfileID = strings.TrimSpace(*patch.PermissionProfileID)
	}
	if patch.DailyLimit != nil {
		entry.DailyLimit = *patch.DailyLimit
	}
	if patch.TotalQuota != nil {
		entry.TotalQuota = *patch.TotalQuota
	}
	if patch.SpendingLimit != nil {
		entry.SpendingLimit = *patch.SpendingLimit
	}
	if periodPatch != nil {
		entry.PeriodSpendingLimits = quota.ApplyPatch(entry.PeriodSpendingLimits, periodPatch)
		entry.DailySpendingLimit = entry.PeriodSpendingLimits.Day
	}
	if patch.ConcurrencyLimit != nil {
		entry.ConcurrencyLimit = *patch.ConcurrencyLimit
	}
	if patch.RPMLimit != nil {
		entry.RPMLimit = *patch.RPMLimit
	}
	if patch.TPMLimit != nil {
		entry.TPMLimit = *patch.TPMLimit
	}
	if patch.AllowedModels != nil {
		entry.AllowedModels = append([]string(nil), (*patch.AllowedModels)...)
	}
	if patch.AllowedChannels != nil {
		entry.AllowedChannels = append([]string(nil), (*patch.AllowedChannels)...)
	}
	if patch.AllowedChannelGroups != nil {
		entry.AllowedChannelGroups = append([]string(nil), (*patch.AllowedChannelGroups)...)
	}
	if patch.SystemPrompt != nil {
		entry.SystemPrompt = strings.TrimSpace(*patch.SystemPrompt)
	}
	if patch.CreatedAt != nil {
		entry.CreatedAt = strings.TrimSpace(*patch.CreatedAt)
	}

	if owned && (patch.Name != nil || periodPatch != nil) {
		if patch.Key != nil || patch.Disabled != nil || patch.CreatedAt != nil {
			return fmt.Errorf("%w: update owned key name/period separately from key state", ErrInvalidEntry)
		}
		svc := enduser.Default()
		if svc == nil && usage.RuntimeDB() != nil {
			svc = enduser.NewService(usage.RuntimeDB())
		}
		if svc == nil {
			return fmt.Errorf("%w: end user service unavailable", ErrInvalidEntry)
		}
		if err := svc.UpdateKey(context.Background(), s.tenantID, existing.EndUserID, existing.ID, patch.Name, periodPatch); err != nil {
			return err
		}
		return nil
	}

	normalized, err := s.prepareEntryForSave(entry.ToConfigEntry())
	if err != nil {
		return err
	}
	desiredKey := strings.TrimSpace(normalized.Key)
	if desiredKey != originalKey {
		if existingKey := usage.GetAPIKeyForTenant(s.tenantID, desiredKey); existingKey != nil && strings.TrimSpace(existingKey.ID) != originalID {
			return ErrDuplicateKey
		}
	}
	updated := usage.APIKeyRowFromConfig(normalized)
	updated.ID = originalID
	// Preserve ownership regardless of client payload (read-only on generic API).
	updated.EndUserID = existing.EndUserID
	updated.IsDefault = existing.IsDefault
	return usage.UpdateAPIKeyByIDForTenant(s.tenantID, updated)
}

func (s *Service) DeleteEntry(key string, id *string, index *int, deleteLogs bool) (DeleteEntryResult, error) {
	targetKey := strings.TrimSpace(key)
	row := s.resolvePatchTargetRow(id, index, &targetKey)
	if row == nil {
		return DeleteEntryResult{}, ErrMissingKeyOrIndex
	}
	targetKey = row.Key
	apiKeyID := strings.TrimSpace(row.ID)
	if err := usage.DeleteAPIKeyByIDForTenant(s.tenantID, row.ID); err != nil {
		return DeleteEntryResult{}, err
	}
	_ = usage.DeleteDailySpendingReset(s.tenantID, apiKeyID)
	_ = usage.DeleteDailySpendingResetEvents(s.tenantID, apiKeyID)

	result := DeleteEntryResult{}
	if deleteLogs && s != nil && s.deleteLogs != nil {
		result.LogsDeleted, _ = s.deleteLogs(targetKey)
	}
	return result, nil
}

func (s *Service) prepareEntryForSave(entry config.APIKeyEntry) (config.APIKeyEntry, error) {
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return config.APIKeyEntry{}, ErrKeyRequired
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.PermissionProfileID = strings.TrimSpace(entry.PermissionProfileID)
	entry.SystemPrompt = strings.TrimSpace(entry.SystemPrompt)
	entry.CreatedAt = strings.TrimSpace(entry.CreatedAt)
	limits, err := quota.NormalizeLimits(entry.PeriodSpendingLimits)
	if err != nil {
		return config.APIKeyEntry{}, err
	}
	day, err := quota.NormalizeWholeUSD(entry.DailySpendingLimit)
	if err != nil {
		return config.APIKeyEntry{}, err
	}
	if limits.Day > 0 && day > 0 && limits.Day != day {
		return config.APIKeyEntry{}, quota.ErrPeriodDayLegacyConflict
	}
	if day == 0 {
		day = limits.Day
	}
	limits.Day = day
	entry.DailySpendingLimit = day
	entry.PeriodSpendingLimits = limits
	entry.AllowedChannelGroups = normalizeChannelGroups(entry.AllowedChannelGroups)

	if s != nil && s.sanitizeChannels != nil {
		cleaned, err := s.sanitizeChannels(entry.AllowedChannels)
		if err != nil {
			return config.APIKeyEntry{}, wrapInvalidEntryError(err)
		}
		entry.AllowedChannels = cleaned
	}
	if s != nil && s.validateChannelGroup != nil {
		validated, err := s.validateChannelGroup(entry.AllowedChannelGroups)
		if err != nil {
			return config.APIKeyEntry{}, wrapInvalidEntryError(err)
		}
		entry.AllowedChannelGroups = validated
	}
	if s != nil && s.validateEntry != nil {
		if err := s.validateEntry(entry); err != nil {
			return config.APIKeyEntry{}, wrapInvalidEntryError(err)
		}
	}

	return entry, nil
}

func (s *Service) resolvePatchTargetRow(id *string, index *int, match *string) *usage.APIKeyRow {
	if id != nil {
		if targetID := strings.TrimSpace(*id); targetID != "" {
			return usage.GetAPIKeyByIDForTenant(s.tenantID, targetID)
		}
	}
	if match != nil {
		if targetKey := strings.TrimSpace(*match); targetKey != "" {
			return usage.GetAPIKeyForTenant(s.tenantID, targetKey)
		}
	}
	if index == nil || *index < 0 {
		return nil
	}
	rows := usage.ListAPIKeysForTenant(s.tenantID)
	if *index >= len(rows) {
		return nil
	}
	row := rows[*index]
	return &row
}

func normalizeChannelGroups(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := internalrouting.NormalizeGroupName(value)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func wrapInvalidEntryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidEntry) || errors.Is(err, ErrKeyRequired) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrInvalidEntry, err.Error())
}

func renameChannelRestrictions(values []string, oldNameSet map[string]struct{}, newName string) ([]string, bool) {
	if len(values) == 0 {
		return values, false
	}
	changed := false
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if shouldRenameChannelRestriction(trimmed, oldNameSet) {
			trimmed = newName
			changed = true
		}
		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; exists {
			changed = true
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, changed
}

func removeChannelRestrictions(values []string, oldNameSet map[string]struct{}) ([]string, bool) {
	if len(values) == 0 {
		return values, false
	}
	changed := false
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if shouldRenameChannelRestriction(trimmed, oldNameSet) {
			changed = true
			continue
		}
		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; exists {
			changed = true
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, changed
}

func shouldRenameChannelRestriction(value string, oldNameSet map[string]struct{}) bool {
	_, exists := oldNameSet[strings.ToLower(strings.TrimSpace(value))]
	return exists
}
