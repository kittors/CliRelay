package auth

import "testing"

func TestSyncPersistedDisabledMirrorsRuntimeFlag(t *testing.T) {
	auth := &Auth{Disabled: true, Metadata: map[string]any{"type": "codex"}}
	SyncPersistedDisabled(auth)
	if auth.Metadata[DisabledMetadataKey] != true {
		t.Fatalf("metadata[%s] = %#v, want true", DisabledMetadataKey, auth.Metadata[DisabledMetadataKey])
	}

	auth.Disabled = false
	SyncPersistedDisabled(auth)
	if auth.Metadata[DisabledMetadataKey] != false {
		t.Fatalf("metadata[%s] = %#v, want false", DisabledMetadataKey, auth.Metadata[DisabledMetadataKey])
	}
}

func TestSyncPersistedDisabledIgnoresNilAuth(t *testing.T) {
	SyncPersistedDisabled(nil)
}

// Config-derived API keys carry neither metadata nor token storage: nothing of
// them is serialized, and creating a metadata map would make the manager start
// writing stray auth files for them.
func TestSyncPersistedDisabledSkipsRecordsWithNothingToSerialize(t *testing.T) {
	auth := &Auth{Disabled: true, Attributes: map[string]string{"api_key": "k"}}
	SyncPersistedDisabled(auth)
	if auth.Metadata != nil {
		t.Fatalf("Metadata = %#v, want nil", auth.Metadata)
	}
}

// An OAuth credential keeps its payload in TokenStorage; the flag still has to
// reach the file the storage writes.
func TestSyncPersistedDisabledSeedsMetadataForTokenStorage(t *testing.T) {
	auth := &Auth{Disabled: true, Storage: stubTokenStorage{}}
	SyncPersistedDisabled(auth)
	if auth.Metadata[DisabledMetadataKey] != true {
		t.Fatalf("metadata[%s] = %#v, want true", DisabledMetadataKey, auth.Metadata[DisabledMetadataKey])
	}
}

type stubTokenStorage struct{}

func (stubTokenStorage) SaveTokenToFile(string) error { return nil }

func TestRestorePersistedDisabled(t *testing.T) {
	cases := []struct {
		name         string
		metadata     map[string]any
		wantDisabled bool
		wantStatus   Status
	}{
		{name: "missing key stays active", metadata: map[string]any{}, wantStatus: StatusActive},
		{name: "explicit false stays active", metadata: map[string]any{"disabled": false}, wantStatus: StatusActive},
		{
			name:         "bool true disables",
			metadata:     map[string]any{"disabled": true},
			wantDisabled: true,
			wantStatus:   StatusDisabled,
		},
		{
			// Hand-edited auth files sometimes carry the flag as a string.
			name:         "string true disables",
			metadata:     map[string]any{"disabled": "true"},
			wantDisabled: true,
			wantStatus:   StatusDisabled,
		},
		{name: "string false stays active", metadata: map[string]any{"disabled": "false"}, wantStatus: StatusActive},
		{name: "unparseable value stays active", metadata: map[string]any{"disabled": "maybe"}, wantStatus: StatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Status: StatusActive, Metadata: tc.metadata}
			RestorePersistedDisabled(auth)
			if auth.Disabled != tc.wantDisabled {
				t.Fatalf("Disabled = %v, want %v", auth.Disabled, tc.wantDisabled)
			}
			if auth.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q", auth.Status, tc.wantStatus)
			}
		})
	}
}

// A metadata-less record must not panic and must not be forced into a state.
func TestRestorePersistedDisabledIgnoresEmptyRecords(t *testing.T) {
	RestorePersistedDisabled(nil)
	auth := &Auth{Status: StatusActive}
	RestorePersistedDisabled(auth)
	if auth.Disabled || auth.Status != StatusActive {
		t.Fatalf("Disabled=%v Status=%q, want active", auth.Disabled, auth.Status)
	}
}

// Round-tripping must not resurrect a stale disable after the credential has
// been re-enabled: the enable path has to overwrite the persisted key.
func TestDisabledPersistenceRoundTrip(t *testing.T) {
	auth := &Auth{Status: StatusActive, Metadata: map[string]any{"disabled": true}}
	RestorePersistedDisabled(auth)
	if !auth.Disabled {
		t.Fatal("expected the persisted flag to disable the auth")
	}

	auth.Disabled = false
	auth.Status = StatusActive
	SyncPersistedDisabled(auth)

	reloaded := &Auth{Status: StatusActive, Metadata: auth.Metadata}
	RestorePersistedDisabled(reloaded)
	if reloaded.Disabled || reloaded.Status != StatusActive {
		t.Fatalf("after re-enable: Disabled=%v Status=%q, want active", reloaded.Disabled, reloaded.Status)
	}
}
