package management

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (h *Handler) renameChannelReferences(oldNames []string, newName string) error {
	newName = strings.TrimSpace(newName)
	oldNameSet := channelRenameSet(oldNames, newName)
	if h == nil || newName == "" || len(oldNameSet) == 0 {
		return nil
	}

	configChanged := false
	routingChanged := false
	if h.cfg != nil {
		if renameRoutingChannelReferences(&h.cfg.Routing, oldNameSet, newName) {
			configChanged = true
			routingChanged = true
		}
		if renameConfigAPIKeyChannels(h.cfg.APIKeyEntries, oldNameSet, newName) {
			configChanged = true
		}
		if renameOAuthModelAliasChannels(h.cfg, oldNameSet, newName) {
			configChanged = true
			if err := usage.UpsertRuntimeSetting(usage.RuntimeSettingOAuthModelAlias, h.cfg.OAuthModelAlias); err != nil {
				return fmt.Errorf("failed to persist oauth model aliases: %w", err)
			}
		}
	}

	if routingChanged && h.cfg != nil {
		if err := usage.UpsertRoutingConfig(h.cfg.Routing); err != nil {
			return fmt.Errorf("failed to persist routing config: %w", err)
		}
	}
	if err := renameSQLiteAPIKeyChannels(oldNameSet, newName); err != nil {
		return err
	}
	if configChanged && h.cfg != nil && strings.TrimSpace(h.configFilePath) != "" {
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}
		if usage.ConfigStoreAvailable() {
			usage.CleanDBBackedConfigFromYAML(h.configFilePath)
		}
	}
	if configChanged && h.authManager != nil {
		h.authManager.SetConfig(h.cfg)
	}
	return nil
}

func channelRenameSet(oldNames []string, newName string) map[string]struct{} {
	newKey := strings.ToLower(strings.TrimSpace(newName))
	oldNameSet := make(map[string]struct{}, len(oldNames))
	for _, oldName := range oldNames {
		oldKey := strings.ToLower(strings.TrimSpace(oldName))
		if oldKey == "" || oldKey == newKey {
			continue
		}
		oldNameSet[oldKey] = struct{}{}
	}
	return oldNameSet
}

func shouldRenameChannel(value string, oldNameSet map[string]struct{}) bool {
	_, exists := oldNameSet[strings.ToLower(strings.TrimSpace(value))]
	return exists
}

func renameChannelList(values []string, oldNameSet map[string]struct{}, newName string) ([]string, bool) {
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
		if shouldRenameChannel(trimmed, oldNameSet) {
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

func renameRoutingChannelReferences(routing *config.RoutingConfig, oldNameSet map[string]struct{}, newName string) bool {
	if routing == nil || len(routing.ChannelGroups) == 0 {
		return false
	}
	changed := false
	for i := range routing.ChannelGroups {
		channels, channelsChanged := renameChannelList(routing.ChannelGroups[i].Match.Channels, oldNameSet, newName)
		if channelsChanged {
			routing.ChannelGroups[i].Match.Channels = channels
			changed = true
		}
		if priorities := routing.ChannelGroups[i].ChannelPriorities; len(priorities) > 0 {
			for channel, priority := range priorities {
				if !shouldRenameChannel(channel, oldNameSet) {
					continue
				}
				delete(priorities, channel)
				if existing, exists := priorities[newName]; !exists || priority > existing {
					priorities[newName] = priority
				}
				changed = true
			}
			if len(priorities) == 0 {
				routing.ChannelGroups[i].ChannelPriorities = nil
			}
		}
	}
	return changed
}

func renameConfigAPIKeyChannels(entries []config.APIKeyEntry, oldNameSet map[string]struct{}, newName string) bool {
	changed := false
	for i := range entries {
		channels, channelsChanged := renameChannelList(entries[i].AllowedChannels, oldNameSet, newName)
		if channelsChanged {
			entries[i].AllowedChannels = channels
			changed = true
		}
	}
	return changed
}

func renameSQLiteAPIKeyChannels(oldNameSet map[string]struct{}, newName string) error {
	for _, row := range usage.ListAPIKeys() {
		channels, changed := renameChannelList(row.AllowedChannels, oldNameSet, newName)
		if !changed {
			continue
		}
		row.AllowedChannels = channels
		if err := usage.UpsertAPIKey(row); err != nil {
			return fmt.Errorf("failed to persist api key channel restrictions: %w", err)
		}
	}
	return nil
}

func renameOAuthModelAliasChannels(cfg *config.Config, oldNameSet map[string]struct{}, newName string) bool {
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return false
	}
	newKey := strings.ToLower(strings.TrimSpace(newName))
	changed := false
	for channel, aliases := range cfg.OAuthModelAlias {
		if !shouldRenameChannel(channel, oldNameSet) {
			continue
		}
		delete(cfg.OAuthModelAlias, channel)
		cfg.OAuthModelAlias[newKey] = append(cfg.OAuthModelAlias[newKey], aliases...)
		changed = true
	}
	if changed {
		cfg.OAuthModelAlias = sanitizedOAuthModelAlias(cfg.OAuthModelAlias)
	}
	return changed
}
