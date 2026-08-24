package auth

// The auth JSON on disk (and its mirror in the database) is the source of truth
// for the enabled/disabled state of a credential. Auth.Disabled is the runtime
// projection of that key, so every persistence backend has to write it back on
// save and read it back on load. Keeping both directions here stops the four
// store implementations from drifting apart again: a store that only wrote the
// runtime field made "disable" survive until the next reload and then silently
// flip back to enabled.
const DisabledMetadataKey = "disabled"

// SyncPersistedDisabled mirrors the runtime Disabled flag into the metadata map
// that stores serialize. Call it in every Store.Save implementation before the
// auth file is written.
//
// A record with neither metadata nor token storage owns no auth file at all —
// config-derived API keys live in config.yaml — so it is left untouched.
// Creating a metadata map for one would make the manager treat it as
// persistable and write a stray JSON file into the auth directory.
func SyncPersistedDisabled(auth *Auth) {
	if auth == nil || (auth.Metadata == nil && auth.Storage == nil) {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any, 1)
	}
	auth.Metadata[DisabledMetadataKey] = auth.Disabled
}

// RestorePersistedDisabled projects the persisted metadata key back onto the
// runtime fields. Call it in every Store.List implementation after an auth
// record has been rebuilt from storage.
//
// Only an explicit true disables the credential: a missing key means the record
// predates the flag and must stay usable.
func RestorePersistedDisabled(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	disabled, ok := metadataBoolValue(auth.Metadata[DisabledMetadataKey])
	if !ok || !disabled {
		return
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
}

// metadataBoolValue accepts the shapes JSON round-trips produce for a boolean:
// real bools, "true"/"false" strings written by hand-edited auth files, and the
// numeric form some older exports used.
func metadataBoolValue(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		switch value {
		case "true", "True", "TRUE", "1", "yes":
			return true, true
		case "false", "False", "FALSE", "0", "no":
			return false, true
		}
	case float64:
		return value != 0, true
	case int:
		return value != 0, true
	case int64:
		return value != 0, true
	}
	return false, false
}
