package management

import (
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// probeMetadata is the selection scope shared by the panel's probes: the model
// test, the image console and the video console.
//
// A probe runs on exactly one picked credential and returns that credential's
// error unchanged, so a failure is the account's own answer and not the last
// step of a retry chain. Any credential of the operator's tenant may be picked.
// Channel groups control what API keys can reach. They should not block an
// operator who is checking whether a model or an account works, and before this
// the default group's excluded models did block it, with
// model_not_allowed_by_channel_group. Callers narrow the returned map further,
// for example to one channel or a pinned account.
func probeMetadata(tenantID string) map[string]any {
	return map[string]any{
		coreexecutor.SinglePickMetadataKey:          true,
		coreexecutor.TenantMetadataKey:              coreauth.NormalizedTenantID(tenantID),
		coreexecutor.TenantWideSelectionMetadataKey: true,
	}
}
