package clusterauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	baseauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// persistedDocument returns the JSON document a save writes for auth, built
// the way FileTokenStore builds the file: the metadata with the routing
// fields and the disabled flag mirrored into it, or the token storage's own
// encoding when a login flow attached one. auth's maps are not modified; the
// manager may hand in the live copy it shares with request goroutines.
func persistedDocument(auth *coreauth.Auth) (map[string]any, error) {
	doc := make(map[string]any, len(auth.Metadata)+4)
	for key, value := range auth.Metadata {
		doc[key] = value
	}
	prefix := strings.TrimSpace(auth.Prefix)
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	proxyID := strings.TrimSpace(auth.ProxyID)
	if prefix != "" {
		doc["prefix"] = prefix
	}
	if proxyURL != "" {
		doc["proxy_url"] = proxyURL
	}
	if proxyID != "" {
		doc["proxy_id"] = proxyID
	}
	doc[coreauth.DisabledMetadataKey] = auth.Disabled
	// A login flow hands over the token storage with partial metadata, and
	// the storage is what the file would hold. Once the credential came back
	// from the store (it carries a version) the metadata is the whole
	// document; the manager keeps the storage attached for good, and encoding
	// it on every request would cost a temporary file under the manager lock.
	if auth.Storage == nil || coreauth.CredentialVersion(auth) > 0 {
		return doc, nil
	}
	baseauth.ApplyMetadata(auth.Storage, doc)
	return encodeStorage(auth.Storage)
}

// encodeStorage captures what a token storage writes to its file. Storages
// only know how to serialise themselves to a path, so they write into a
// private temporary directory that is removed right after.
func encodeStorage(storage coreauth.TokenStorage) (map[string]any, error) {
	dir, err := os.MkdirTemp("", "clirelay-credential-*")
	if err != nil {
		return nil, fmt.Errorf("prepare credential encoding: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "credential.json")
	if err = storage.SaveTokenToFile(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encoded credential: %w", err)
	}
	doc := make(map[string]any)
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode encoded credential: %w", err)
	}
	return doc, nil
}

// splitDocument separates the credential from the runtime observations and
// drops the version stamp, which is the store's to set.
func splitDocument(doc map[string]any) (content, runtime map[string]any) {
	content = make(map[string]any, len(doc))
	runtime = make(map[string]any)
	for key, value := range doc {
		switch {
		case key == coreauth.CredentialVersionMetadataKey:
		case coreauth.IsRuntimeMetadataKey(key):
			runtime[key] = value
		default:
			content[key] = value
		}
	}
	return content, runtime
}

// canonicalJSON encodes v so that equal documents compare equal byte for byte
// whether they came from Go values or from JSON: keys are sorted and numbers
// take their decoded float64 form.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// canonicalEntries encodes each value of m on its own.
func canonicalEntries(m map[string]any) (map[string][]byte, error) {
	out := make(map[string][]byte, len(m))
	for key, value := range m {
		raw, err := canonicalJSON(value)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", key, err)
		}
		out[key] = raw
	}
	return out, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	out := make(map[string]any)
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make(map[string]any)
	}
	return out, nil
}

func decodeEntries(raw []byte) (map[string][]byte, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	return canonicalEntries(obj)
}

func contentDigest(content []byte) [32]byte {
	return sha256.Sum256(content)
}

// mirrorDocument assembles what the mirror file of a credential holds: the
// document the file store would have written, stamped with its version.
func mirrorDocument(content []byte, runtime map[string][]byte, version int64) ([]byte, error) {
	doc, err := decodeObject(content)
	if err != nil {
		return nil, err
	}
	for key, raw := range runtime {
		var value any
		if err = json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		doc[key] = value
	}
	doc[coreauth.CredentialVersionMetadataKey] = version
	return json.Marshal(doc)
}

// authFromRow builds the Auth the manager gets for a stored credential,
// exactly as FileTokenStore builds it from the equivalent file.
func authFromRow(r row, path string) (*coreauth.Auth, error) {
	metadata, err := decodeObject(r.Content)
	if err != nil {
		return nil, fmt.Errorf("decode credential %s: %w", r.ID, err)
	}
	runtime, err := decodeObject(r.Runtime)
	if err != nil {
		return nil, fmt.Errorf("decode runtime of %s: %w", r.ID, err)
	}
	for key, value := range runtime {
		metadata[key] = value
	}
	metadata[coreauth.CredentialVersionMetadataKey] = float64(r.Version)
	provider := sdkauth.InferAuthProvider(metadata)
	auth := sdkauth.NewFileAuthRecord(r.ID, path, provider, metadata, r.UpdatedAt)
	if !r.CreatedAt.IsZero() {
		auth.CreatedAt = r.CreatedAt
	}
	return auth, nil
}

func documentProvider(content map[string]any) string {
	probe := make(map[string]any, len(content))
	for key, value := range content {
		probe[key] = value
	}
	return sdkauth.InferAuthProvider(probe)
}
