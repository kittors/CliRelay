package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Credential versioning hooks.
//
// A cluster deployment keeps credentials in a shared database, versions every
// write and mirrors each document into the local auth directory so the file
// watcher, downloads and OAuth callbacks keep working on files. The interfaces
// below let such a store plug into the Manager without the SDK depending on
// it. The default file store implements none of them, and every code path that
// consults them keeps its single-node behaviour when they are absent.

// CredentialVersionMetadataKey is the reserved metadata key carrying the
// persisted version of a credential document. Versioned stores stamp it on
// the documents and mirror files they produce; the file store never sets it,
// so single-node credentials always report version 0.
const CredentialVersionMetadataKey = "_clirelay_version"

var (
	// ErrCredentialConflict reports that a credential write was based on an
	// older version than the persisted one and was not applied. Re-read the
	// credential and apply the change again.
	ErrCredentialConflict = errors.New("auth: credential was changed concurrently")
	// ErrCredentialGone reports that the credential was deleted, or is not
	// known to the store. A versioned store never brings it back implicitly.
	ErrCredentialGone = errors.New("auth: credential no longer exists")
	// ErrRefreshInProgress reports that another node holds the refresh lease
	// of a credential and did not release it in time.
	ErrRefreshInProgress = errors.New("auth: credential refresh in progress on another node")
)

// VersionedStore is a Store that versions credential documents. Writes based
// on an outdated version fail with ErrCredentialConflict instead of replacing
// newer data.
type VersionedStore interface {
	Store
	// Get returns the newest persisted copy of id, or nil when the credential
	// does not exist or has been deleted.
	Get(ctx context.Context, id string) (*Auth, error)
}

// AuthIndexResolver lets a store own auth_index assignment. auth_index keys
// request logs, quota snapshots and account bindings, so every node must give
// a credential the same value no matter which code path registered it.
type AuthIndexResolver interface {
	// ResolveAuthIndex returns the auth_index for the persisted credential id,
	// or "" when the store has no opinion.
	ResolveAuthIndex(id string) string
}

// RefreshCoordinator serialises credential refreshes across processes that
// share one credential store, so a single-use refresh token is spent by one
// node only. Single-node deployments leave it unset.
type RefreshCoordinator interface {
	// ClaimRefresh tries to take the refresh lease of id. acquired is false
	// when another node holds it; that node publishes its result. latest is
	// the newest persisted copy of the credential, when known.
	ClaimRefresh(ctx context.Context, id string) (latest *Auth, acquired bool, err error)
	// ReleaseRefresh gives the lease back once the outcome was persisted.
	ReleaseRefresh(ctx context.Context, id string)
}

// runtimeMetadataKeys are metadata entries that record what a node observed
// while serving traffic (health, probe-confirmed quota gates, project probe
// back-off) rather than the credential itself. A versioned store keeps them
// apart from the credential and never bumps the version for them, so request
// bookkeeping can not race a token rotation. The antigravity names mirror the
// constants in internal/auth/antigravity, which the SDK cannot import.
var runtimeMetadataKeys = []string{
	ClaudeOAuthHealthMetadataKey,
	persistedQuotaRuntimeMetadataKey,
	"antigravity_project_id_unavailable",
	"antigravity_project_id_probed_at",
	"antigravity_project_id_probe_err",
}

// RuntimeMetadataKeys returns the metadata keys that hold runtime observations.
func RuntimeMetadataKeys() []string {
	return append([]string(nil), runtimeMetadataKeys...)
}

// IsRuntimeMetadataKey reports whether key holds a runtime observation.
func IsRuntimeMetadataKey(key string) bool {
	for _, candidate := range runtimeMetadataKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

// CredentialVersion returns the persisted version recorded in auth's metadata,
// or 0 when the credential is not versioned.
func CredentialVersion(auth *Auth) int64 {
	if auth == nil {
		return 0
	}
	return MetadataCredentialVersion(auth.Metadata)
}

// MetadataCredentialVersion reads CredentialVersionMetadataKey from metadata.
// JSON round trips turn the number into float64, so every numeric shape counts.
func MetadataCredentialVersion(metadata map[string]any) int64 {
	raw, ok := metadata[CredentialVersionMetadataKey]
	if !ok || raw == nil {
		return 0
	}
	switch value := raw.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case int32:
		return int64(value)
	case float64:
		return int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0
		}
		return parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// FileAuthIndex returns the auth_index a file-backed credential gets when the
// file store loads it at startup: the seed is its auth-dir relative path, the
// same value readAuthFile puts in FileName. Versioned stores use it for new
// credentials so the index is what a single node would have assigned.
func FileAuthIndex(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return stableAuthIndex("file:" + id)
}

type credentialCreateContextKey struct{}

// WithCredentialCreate marks ctx as an explicit create-or-replace of a
// credential: an upload, an OAuth or CLI login, an import. A versioned store
// only lets such a write add a credential or bring back a deleted one; a late
// write from anywhere else never resurrects it. The file store ignores it.
func WithCredentialCreate(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, credentialCreateContextKey{}, true)
}

// IsCredentialCreate reports whether ctx was marked by WithCredentialCreate.
func IsCredentialCreate(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(credentialCreateContextKey{}).(bool)
	return marked
}

type resultPersistContextKey struct{}

func withResultPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, resultPersistContextKey{}, true)
}

// IsResultPersist reports whether a save records a request result. The manager
// holds its global lock during that save, so a store must not block on remote
// I/O there; it may defer the write instead.
func IsResultPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(resultPersistContextKey{}).(bool)
	return marked
}

// PreserveRuntimeMetadata makes dst carry exactly the runtime metadata entries
// of src. Reloading an unchanged credential from its mirror file uses it so
// the node's own, newer observations are not rolled back to the file's copy.
func PreserveRuntimeMetadata(dst, src *Auth) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range runtimeMetadataKeys {
		value, ok := src.Metadata[key]
		if ok {
			if dst.Metadata == nil {
				dst.Metadata = make(map[string]any)
			}
			dst.Metadata[key] = value
			continue
		}
		delete(dst.Metadata, key)
	}
}

// MetadataSnapshot is a point-in-time copy of credential metadata holding one
// encoded value per key. A refresh takes it first so the keys the refresh
// changed can be re-applied onto a newer copy if the write conflicts.
type MetadataSnapshot map[string][]byte

// SnapshotMetadata encodes every metadata entry except the version stamp.
// Values are encoded immediately because refresh code mutates nested maps in
// place, which a shallow copy would not survive.
func SnapshotMetadata(metadata map[string]any) MetadataSnapshot {
	snapshot := make(MetadataSnapshot, len(metadata))
	for key, value := range metadata {
		if key == CredentialVersionMetadataKey {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		snapshot[key] = raw
	}
	return snapshot
}

// changes returns the entries of after that differ from the snapshot and the
// keys the snapshot had that after no longer has.
func (s MetadataSnapshot) changes(after map[string]any) (map[string]any, []string) {
	set := make(map[string]any)
	for key, value := range after {
		if key == CredentialVersionMetadataKey {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		if previous, ok := s[key]; ok && bytes.Equal(previous, raw) {
			continue
		}
		set[key] = value
	}
	var removed []string
	for key := range s {
		if _, ok := after[key]; !ok {
			removed = append(removed, key)
		}
	}
	return set, removed
}
