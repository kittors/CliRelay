package authfiles

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// FieldPatch carries editable auth-file fields from the management API into
// the authfiles use case layer.
type FieldPatch struct {
	Name                       string    `json:"name"`
	Label                      *string   `json:"label"`
	CustomTags                 *[]string `json:"custom_tags"`
	HiddenDefaultTags          *[]string `json:"hidden_default_tags"`
	DisplayTags                *[]string `json:"display_tags"`
	Prefix                     *string   `json:"prefix"`
	ProxyURL                   *string   `json:"proxy_url"`
	ProxyID                    *string   `json:"proxy_id"`
	Priority                   *int      `json:"priority"`
	SubscriptionStartedAt      *string   `json:"subscription_started_at"`
	SubscriptionPeriod         *string   `json:"subscription_period"`
	SubscriptionExpiresAt      *string   `json:"subscription_expires_at"`
	CodexCLIOnly               *bool     `json:"codex_cli_only"`
	CodexCLIOnlyAllowedClients *[]string `json:"codex_cli_only_allowed_clients"`
}

type FieldPatchOptions struct {
	Now           time.Time
	ValidateLabel func(label, excludeAuthID string) (string, error)
}

type FieldPatchResult struct {
	OldChannelIdentifiers []string
	NewChannelLabel       string
}

func ApplyStatusPatch(auth *coreauth.Auth, disabled bool, now time.Time) error {
	if auth == nil {
		return fmt.Errorf("auth file not found")
	}
	if now.IsZero() {
		now = time.Now()
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "disabled via management API"
	} else {
		auth.Status = coreauth.StatusActive
		auth.StatusMessage = ""
	}
	auth.UpdatedAt = now
	return nil
}

// ApplyFieldPatch applies editable auth-file field changes and returns the
// channel rename side effect that the transport layer must coordinate.
func ApplyFieldPatch(auth *coreauth.Auth, patch FieldPatch, opts FieldPatchOptions) (FieldPatchResult, error) {
	var result FieldPatchResult
	if auth == nil {
		return result, fmt.Errorf("auth file not found")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	changed := false
	if patch.Label != nil {
		accountType, _ := auth.AccountInfo()
		if !strings.EqualFold(accountType, "oauth") {
			return result, fmt.Errorf("label is only supported for oauth auth files")
		}
		if IsRuntimeOnly(auth) {
			return result, fmt.Errorf("runtime-only auth files cannot rename channel")
		}
		validateLabel := opts.ValidateLabel
		if validateLabel == nil {
			validateLabel = defaultValidateLabel
		}
		label, errValidate := validateLabel(*patch.Label, auth.ID)
		if errValidate != nil {
			return result, errValidate
		}
		result.OldChannelIdentifiers = auth.ChannelIdentifiers()
		result.NewChannelLabel = label
		auth.Label = label
		metadata := ensureMetadata(auth)
		metadata["label"] = label
		changed = true
	}
	if patch.Prefix != nil {
		auth.Prefix = strings.TrimSpace(*patch.Prefix)
		metadata := ensureMetadata(auth)
		if auth.Prefix == "" {
			delete(metadata, "prefix")
		} else {
			metadata["prefix"] = auth.Prefix
		}
		changed = true
	}
	if patch.CustomTags != nil {
		tags, errNormalize := NormalizeEditableTags(*patch.CustomTags, MaxCustomTags)
		if errNormalize != nil {
			return result, errNormalize
		}
		metadata := ensureMetadata(auth)
		if len(tags) == 0 {
			delete(metadata, "custom_tags")
		} else {
			metadata["custom_tags"] = tags
		}
		changed = true
	}
	if patch.HiddenDefaultTags != nil {
		tags := NormalizeTagList(*patch.HiddenDefaultTags)
		metadata := ensureMetadata(auth)
		if len(tags) == 0 {
			delete(metadata, "hidden_default_tags")
		} else {
			metadata["hidden_default_tags"] = tags
		}
		changed = true
	}
	if patch.DisplayTags != nil {
		tags := NormalizeTagList(*patch.DisplayTags)
		metadata := ensureMetadata(auth)
		metadata["display_tags"] = tags
		changed = true
	}
	if patch.ProxyURL != nil {
		auth.ProxyURL = strings.TrimSpace(*patch.ProxyURL)
		metadata := ensureMetadata(auth)
		if auth.ProxyURL == "" {
			delete(metadata, "proxy_url")
			delete(metadata, "proxy-url")
			delete(metadata, "proxyUrl")
		} else {
			metadata["proxy_url"] = auth.ProxyURL
		}
		changed = true
	}
	if patch.ProxyID != nil {
		auth.ProxyID = strings.TrimSpace(*patch.ProxyID)
		metadata := ensureMetadata(auth)
		if auth.ProxyID == "" {
			delete(metadata, "proxy_id")
		} else {
			metadata["proxy_id"] = auth.ProxyID
		}
		changed = true
	}
	if patch.Priority != nil {
		metadata := ensureMetadata(auth)
		if *patch.Priority == 0 {
			delete(metadata, "priority")
		} else {
			metadata["priority"] = *patch.Priority
		}
		changed = true
	}
	if patch.SubscriptionStartedAt != nil {
		value := strings.TrimSpace(*patch.SubscriptionStartedAt)
		metadata := ensureMetadata(auth)
		if value == "" {
			ClearSubscriptionMetadata(metadata)
		} else {
			ts, ok := ParseSubscriptionTimestampValue(value)
			if !ok || ts.IsZero() {
				return result, fmt.Errorf("subscription_started_at must be a valid time")
			}
			DeleteSubscriptionStartMetadata(metadata)
			DeleteSubscriptionExpirationMetadata(metadata)
			metadata["subscription_started_at"] = ts.UTC().Format(time.RFC3339)
			if patch.SubscriptionPeriod == nil {
				if period, okPeriod := ExtractSubscriptionPeriod(metadata); okPeriod {
					metadata["subscription_period"] = period
					delete(metadata, "subscriptionPeriod")
				} else {
					metadata["subscription_period"] = "monthly"
				}
			}
		}
		changed = true
	}
	if patch.SubscriptionPeriod != nil {
		value := strings.TrimSpace(*patch.SubscriptionPeriod)
		metadata := ensureMetadata(auth)
		if value == "" {
			DeleteSubscriptionPeriodMetadata(metadata)
		} else {
			period, ok := NormalizeSubscriptionPeriodValue(value)
			if !ok {
				return result, fmt.Errorf("subscription_period must be monthly or yearly")
			}
			DeleteSubscriptionPeriodMetadata(metadata)
			metadata["subscription_period"] = period
		}
		changed = true
	}
	if patch.SubscriptionExpiresAt != nil {
		value := strings.TrimSpace(*patch.SubscriptionExpiresAt)
		metadata := ensureMetadata(auth)
		if value == "" {
			ClearSubscriptionMetadata(metadata)
		} else {
			ts, ok := ParseSubscriptionTimestampValue(value)
			if !ok || ts.IsZero() {
				return result, fmt.Errorf("subscription_expires_at must be a valid time")
			}
			DeleteSubscriptionStartMetadata(metadata)
			DeleteSubscriptionPeriodMetadata(metadata)
			DeleteSubscriptionExpirationMetadata(metadata)
			metadata["subscription_expires_at"] = ts.UTC().Format(time.RFC3339)
		}
		changed = true
	}
	if patch.CodexCLIOnly != nil {
		if err := ensureCodexOAuthAdmissionEditable(auth); err != nil {
			return result, err
		}
		metadata := ensureMetadata(auth)
		metadata["codex_cli_only"] = *patch.CodexCLIOnly
		changed = true
	}
	if patch.CodexCLIOnlyAllowedClients != nil {
		if err := ensureCodexOAuthAdmissionEditable(auth); err != nil {
			return result, err
		}
		allowedClients, errNormalize := codexadmission.NormalizeAllowedClientPresets(*patch.CodexCLIOnlyAllowedClients)
		if errNormalize != nil {
			return result, errNormalize
		}
		metadata := ensureMetadata(auth)
		if len(allowedClients) == 0 {
			delete(metadata, "codex_cli_only_allowed_clients")
		} else {
			metadata["codex_cli_only_allowed_clients"] = allowedClients
		}
		changed = true
	}

	if !changed {
		return result, fmt.Errorf("no fields to update")
	}
	auth.UpdatedAt = now
	return result, nil
}

func defaultValidateLabel(label, excludeAuthID string) (string, error) {
	_ = excludeAuthID
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return "", fmt.Errorf("channel name is required")
	}
	return trimmed, nil
}

func ensureMetadata(auth *coreauth.Auth) map[string]any {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	return auth.Metadata
}
