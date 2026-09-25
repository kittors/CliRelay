package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func versionedAuth(version any, extra map[string]any) *coreauth.Auth {
	metadata := map[string]any{"type": "claude"}
	if version != nil {
		metadata[coreauth.CredentialVersionMetadataKey] = version
	}
	for k, v := range extra {
		metadata[k] = v
	}
	return &coreauth.Auth{ID: "t/a.json", Metadata: metadata}
}

func TestAcceptCredentialReloadIgnoresOlderMirrorCopies(t *testing.T) {
	existing := versionedAuth(float64(5), nil)
	if acceptCredentialReload(versionedAuth(float64(4), nil), existing) {
		t.Fatal("an older mirror copy must not roll the credential back")
	}
	if acceptCredentialReload(versionedAuth(nil, nil), existing) {
		t.Fatal("an unstamped file must not replace a versioned credential")
	}
	if !acceptCredentialReload(versionedAuth(float64(6), nil), existing) {
		t.Fatal("a newer copy must be applied")
	}
}

func TestAcceptCredentialReloadKeepsRuntimeObservationsOnSameVersion(t *testing.T) {
	existing := versionedAuth(float64(5), map[string]any{coreauth.ClaudeOAuthHealthMetadataKey: "node-view"})
	incoming := versionedAuth(float64(5), map[string]any{coreauth.ClaudeOAuthHealthMetadataKey: "file-view"})
	if !acceptCredentialReload(incoming, existing) {
		t.Fatal("same version must still be applied (forced reloads re-register models)")
	}
	if incoming.Metadata[coreauth.ClaudeOAuthHealthMetadataKey] != "node-view" {
		t.Fatalf("runtime key = %v, want this node's own observation", incoming.Metadata[coreauth.ClaudeOAuthHealthMetadataKey])
	}
}

func TestAcceptCredentialReloadIsInertForSingleNodeFiles(t *testing.T) {
	existing := versionedAuth(nil, map[string]any{coreauth.ClaudeOAuthHealthMetadataKey: "memory"})
	incoming := versionedAuth(nil, map[string]any{coreauth.ClaudeOAuthHealthMetadataKey: "file"})
	if !acceptCredentialReload(incoming, existing) || incoming.Metadata[coreauth.ClaudeOAuthHealthMetadataKey] != "file" {
		t.Fatal("unversioned (single-node) reloads must pass through untouched")
	}
}
