package enduser

import "strings"

// appendPermissionProfileSets writes the permission-profile change, and clears
// what the profile owned when it is being unbound.
//
// Binding a profile copies its allowed-models / allowed-channels /
// allowed-channel-groups / system-prompt onto the account row. Unbinding used to
// reset only the quota fields and leave those four behind. The console renders
// them only while a profile is attached, so the account then read "unrestricted"
// while a stale channel-group whitelist was still in force — invisible in the
// UI, unreachable from it, and unnamed by the request error.
//
// The cleanup fires only on set → empty: switching between profiles is a rebind,
// not an unbind. An explicit value in the same patch still wins, so this only
// fills fields the caller left alone.
func appendPermissionProfileSets(
	sets []string,
	args []interface{},
	currentProfileID string,
	patch *QuotaPatch,
) ([]string, []interface{}) {
	nextProfileID := strings.TrimSpace(*patch.PermissionProfileID)
	sets = append(sets, "permission_profile_id = ?")
	args = append(args, nextProfileID)

	if nextProfileID != "" || strings.TrimSpace(currentProfileID) == "" {
		return sets, args
	}
	if patch.AllowedModels == nil {
		sets = append(sets, "allowed_models = ?")
		args = append(args, encodeJSONStringList(nil))
	}
	if patch.AllowedChannels == nil {
		sets = append(sets, "allowed_channels = ?")
		args = append(args, encodeJSONStringList(nil))
	}
	if patch.AllowedChannelGroups == nil {
		sets = append(sets, "allowed_channel_groups = ?")
		args = append(args, encodeJSONStringList(nil))
	}
	if patch.SystemPrompt == nil {
		sets = append(sets, "system_prompt = ?")
		args = append(args, "")
	}
	return sets, args
}
