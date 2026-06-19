package apikey

import (
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

var (
	ErrInvalidProfileID   = errors.New("id is required")
	ErrInvalidProfileName = errors.New("name is required")
	ErrMissingValue       = errors.New("missing value")
	ErrInvalidEntry       = errors.New("invalid api key entry")
	ErrItemNotFound       = errors.New("item not found")
	ErrDuplicateKey       = errors.New("api key already exists")
	ErrMissingKeyOrIndex  = errors.New("missing key or index")
	ErrKeyRequired        = errors.New("key is required")
)

type ChannelSanitizer func([]string) ([]string, error)
type ChannelGroupValidator func([]string) ([]string, error)
type EntryValidator func(config.APIKeyEntry) error
type LogsDeleter func(string) (int64, error)
type Option func(*Service)

type Service struct {
	sanitizeChannels     ChannelSanitizer
	validateChannelGroup ChannelGroupValidator
	validateEntry        EntryValidator
	deleteLogs           LogsDeleter
}

type EntryPatch struct {
	Key                  *string   `json:"key"`
	Name                 *string   `json:"name"`
	PermissionProfileID  *string   `json:"permission-profile-id"`
	DailyLimit           *int      `json:"daily-limit"`
	TotalQuota           *int      `json:"total-quota"`
	SpendingLimit        *float64  `json:"spending-limit"`
	DailySpendingLimit   *float64  `json:"daily-spending-limit"`
	ConcurrencyLimit     *int      `json:"concurrency-limit"`
	RPMLimit             *int      `json:"rpm-limit"`
	TPMLimit             *int      `json:"tpm-limit"`
	AllowedModels        *[]string `json:"allowed-models"`
	AllowedChannels      *[]string `json:"allowed-channels"`
	AllowedChannelGroups *[]string `json:"allowed-channel-groups"`
	SystemPrompt         *string   `json:"system-prompt"`
	CreatedAt            *string   `json:"created-at"`
}

type DeleteEntryResult struct {
	LogsDeleted int64
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
	rows := usage.ListAPIKeys()
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if !row.Disabled {
			keys = append(keys, row.Key)
		}
	}
	return keys
}

func (s *Service) ListRows() []usage.APIKeyRow {
	return usage.ListAPIKeys()
}

func (s *Service) GetRow(key string) *usage.APIKeyRow {
	return usage.GetAPIKey(strings.TrimSpace(key))
}

func (s *Service) GetRowByID(id string) *usage.APIKeyRow {
	return usage.GetAPIKeyByID(strings.TrimSpace(id))
}

func (s *Service) ReplaceKeys(keys []string) error {
	rows := make([]usage.APIKeyRow, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			rows = append(rows, usage.APIKeyRow{Key: trimmed})
		}
	}
	return usage.ReplaceAllAPIKeys(rows)
}

func (s *Service) PatchKey(oldKey string, newKey string) error {
	oldKey = strings.TrimSpace(oldKey)
	newKey = strings.TrimSpace(newKey)
	if oldKey == "" {
		if newKey == "" {
			return nil
		}
		return usage.UpsertAPIKey(usage.APIKeyRow{Key: newKey})
	}
	existing := usage.GetAPIKey(oldKey)
	if existing == nil {
		if newKey == "" {
			return nil
		}
		return usage.UpsertAPIKey(usage.APIKeyRow{Key: newKey})
	}
	if newKey == "" {
		return usage.DeleteAPIKeyByID(existing.ID)
	}
	updated := *existing
	updated.Key = newKey
	return usage.UpdateAPIKeyByID(updated)
}

func (s *Service) DeleteKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrMissingValue
	}
	return usage.DeleteAPIKey(key)
}

func (s *Service) PermissionProfiles() []usage.APIKeyPermissionProfileRow {
	return usage.ListAPIKeyPermissionProfiles()
}

func (s *Service) ReplacePermissionProfiles(profiles []usage.APIKeyPermissionProfileRow) error {
	normalized := make([]usage.APIKeyPermissionProfileRow, len(profiles))
	copy(normalized, profiles)
	for idx := range normalized {
		normalized[idx].ID = strings.TrimSpace(normalized[idx].ID)
		normalized[idx].Name = strings.TrimSpace(normalized[idx].Name)
		if normalized[idx].ID == "" {
			return ErrInvalidProfileID
		}
		if normalized[idx].Name == "" {
			return ErrInvalidProfileName
		}
		if s != nil && s.sanitizeChannels != nil {
			cleaned, err := s.sanitizeChannels(normalized[idx].AllowedChannels)
			if err != nil {
				return err
			}
			normalized[idx].AllowedChannels = cleaned
		}
	}
	return usage.ReplaceAllAPIKeyPermissionProfiles(normalized)
}

func (s *Service) RenameAllowedChannelRestrictions(oldNameSet map[string]struct{}, newName string) error {
	for _, row := range usage.ListAPIKeys() {
		channels, changed := renameChannelRestrictions(row.AllowedChannels, oldNameSet, newName)
		if !changed {
			continue
		}
		row.AllowedChannels = channels
		if err := usage.UpsertAPIKey(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RemoveAllowedChannelRestrictions(oldNameSet map[string]struct{}) error {
	for _, row := range usage.ListAPIKeys() {
		channels, changed := removeChannelRestrictions(row.AllowedChannels, oldNameSet)
		if !changed {
			continue
		}
		row.AllowedChannels = channels
		if err := usage.UpsertAPIKey(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RenamePermissionProfileChannelRestrictions(oldNameSet map[string]struct{}, newName string) error {
	profiles := usage.ListAPIKeyPermissionProfiles()
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
	profiles := usage.ListAPIKeyPermissionProfiles()
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
	rows := usage.EffectiveAPIKeyRows(usage.ListAPIKeys())
	entries := make([]config.APIKeyEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, row.ToConfigEntry())
	}
	return entries
}

func (s *Service) ReplaceEntries(entries []config.APIKeyEntry) error {
	rows := make([]usage.APIKeyRow, 0, len(entries))
	for _, entry := range entries {
		normalized, err := s.prepareEntryForSave(entry)
		if err != nil {
			return err
		}
		rows = append(rows, usage.APIKeyRowFromConfig(normalized))
	}
	return usage.ReplaceAllAPIKeys(rows)
}

func (s *Service) PatchEntry(id *string, index *int, match *string, patch EntryPatch) error {
	existing := resolvePatchTargetRow(id, index, match)
	if existing == nil {
		return ErrItemNotFound
	}
	entry := *existing
	originalKey := strings.TrimSpace(entry.Key)
	originalID := strings.TrimSpace(entry.ID)

	if patch.Key != nil {
		entry.Key = strings.TrimSpace(*patch.Key)
	}
	if patch.Name != nil {
		entry.Name = strings.TrimSpace(*patch.Name)
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
	if patch.DailySpendingLimit != nil {
		entry.DailySpendingLimit = *patch.DailySpendingLimit
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

	normalized, err := s.prepareEntryForSave(entry.ToConfigEntry())
	if err != nil {
		return err
	}
	desiredKey := strings.TrimSpace(normalized.Key)
	if desiredKey != originalKey {
		if existingKey := usage.GetAPIKey(desiredKey); existingKey != nil && strings.TrimSpace(existingKey.ID) != originalID {
			return ErrDuplicateKey
		}
	}
	updated := usage.APIKeyRowFromConfig(normalized)
	updated.ID = originalID
	return usage.UpdateAPIKeyByID(updated)
}

func (s *Service) DeleteEntry(key string, id *string, index *int, deleteLogs bool) (DeleteEntryResult, error) {
	targetKey := strings.TrimSpace(key)
	row := resolvePatchTargetRow(id, index, &targetKey)
	if row == nil {
		return DeleteEntryResult{}, ErrMissingKeyOrIndex
	}
	targetKey = row.Key
	if err := usage.DeleteAPIKeyByID(row.ID); err != nil {
		return DeleteEntryResult{}, err
	}

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

func resolvePatchTargetRow(id *string, index *int, match *string) *usage.APIKeyRow {
	if id != nil {
		if targetID := strings.TrimSpace(*id); targetID != "" {
			return usage.GetAPIKeyByID(targetID)
		}
	}
	if match != nil {
		if targetKey := strings.TrimSpace(*match); targetKey != "" {
			return usage.GetAPIKey(targetKey)
		}
	}
	if index == nil || *index < 0 {
		return nil
	}
	rows := usage.ListAPIKeys()
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
