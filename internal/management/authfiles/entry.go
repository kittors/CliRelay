package authfiles

import (
	"os"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type EntryOptions struct {
	Now         time.Time
	Stat        func(string) (os.FileInfo, error)
	OnStatError func(string, error)
}

func ListEntries(auths []*coreauth.Auth, opts EntryOptions) []map[string]any {
	files := make([]map[string]any, 0, len(auths))
	for _, auth := range auths {
		if entry := BuildEntry(auth, opts); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	return files
}

// BuildEntry returns the public management auth-file entry for an auth record.
// It preserves legacy response fields while keeping file-stat side effects
// injected by the transport wrapper.
func BuildEntry(auth *coreauth.Auth, opts EntryOptions) map[string]any {
	if auth == nil {
		return nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	stat := opts.Stat
	if stat == nil {
		stat = os.Stat
	}

	auth.EnsureIndex()
	runtimeOnly := IsRuntimeOnly(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(Attribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := map[string]any{
		"id":             auth.ID,
		"auth_index":     auth.Index,
		"name":           name,
		"type":           strings.TrimSpace(auth.Provider),
		"provider":       strings.TrimSpace(auth.Provider),
		"label":          auth.ChannelName(),
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"runtime_only":   runtimeOnly,
		"source":         "memory",
		"size":           int64(0),
	}
	if email := Email(auth); email != "" {
		entry["email"] = email
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	tags := BuildTagPayload(auth)
	entry["default_tags"] = tags.DefaultTags
	entry["custom_tags"] = tags.CustomTags
	entry["hidden_default_tags"] = tags.HiddenDefaultTags
	entry["display_tags"] = tags.DisplayTags
	if planType := NormalizeTagValue(MetadataString(auth.Metadata, "plan_type", "planType")); planType != "" {
		entry["plan_type"] = planType
	}
	AddSubscriptionFields(entry, auth.Metadata, now)
	if health := ClaudeOAuthHealth(auth); len(health) > 0 {
		entry["claude_oauth_health"] = health
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if restrictions := BuildRestrictionPayload(auth, now); len(restrictions) > 0 {
		entry["restrictions"] = restrictions
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if info, err := stat(path); err == nil {
			entry["size"] = info.Size()
			entry["modtime"] = info.ModTime()
		} else if os.IsNotExist(err) {
			// Hide credentials removed from disk but still lingering in memory.
			if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
				return nil
			}
			entry["source"] = "memory"
		} else if opts.OnStatError != nil {
			opts.OnStatError(path, err)
		}
	}
	if claims := CodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	if admission := CodexOAuthAdmissionPayload(auth); len(admission) > 0 {
		entry["codex_oauth_admission"] = admission
		entry["codex_cli_only"] = admission["enabled"]
		entry["codex_cli_only_allowed_clients"] = admission["allowed_clients"]
	}
	return entry
}
