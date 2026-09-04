package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	baseauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu      sync.Mutex
	dirLock sync.RWMutex
	baseDir string
}

const (
	projectIDFetchFailedAtKey = "project_id_fetch_failed_at"
	projectIDFetchCooldown    = time.Hour
)

var fetchAntigravityProjectID = FetchAntigravityProjectID

// NewFileTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewFileTokenStore() *FileTokenStore {
	return &FileTokenStore{}
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *FileTokenStore) SetBaseDir(dir string) {
	s.dirLock.Lock()
	s.baseDir = strings.TrimSpace(dir)
	s.dirLock.Unlock()
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *FileTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("auth filestore: create dir failed: %w", err)
	}
	syncRoutingMetadata(auth)
	// Both branches serialize auth.Metadata, so the disabled flag has to be
	// mirrored before either of them runs; the Storage branch used to skip it,
	// which lost every enable/disable made on an OAuth credential.
	cliproxyauth.SyncPersistedDisabled(auth)

	switch {
	case auth.Storage != nil:
		baseauth.ApplyMetadata(auth.Storage, auth.Metadata)
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
			file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
			if errOpen != nil {
				return "", fmt.Errorf("auth filestore: open existing failed: %w", errOpen)
			}
			if _, errWrite := file.Write(raw); errWrite != nil {
				_ = file.Close()
				return "", fmt.Errorf("auth filestore: write existing failed: %w", errWrite)
			}
			if errClose := file.Close(); errClose != nil {
				return "", fmt.Errorf("auth filestore: close existing failed: %w", errClose)
			}
			return path, nil
		} else if !os.IsNotExist(errRead) {
			return "", fmt.Errorf("auth filestore: read existing failed: %w", errRead)
		}
		if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("auth filestore: write file failed: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	return path, nil
}

// List enumerates all auth JSON files under the configured directory.
func (s *FileTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		auth, err := s.readAuthFile(path, dir)
		if err != nil {
			return nil
		}
		if auth != nil {
			entries = append(entries, auth)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Delete removes the auth file.
func (s *FileTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	return nil
}

func (s *FileTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *FileTokenStore) readAuthFile(path, baseDir string) (*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider := InferAuthProvider(metadata)
	// Normalize on every disk load so imported OAuth JSON (xai/claude/kimi/gemini/codex)
	// converges to the same shape as a fresh login after restart or cross-tenant copy.
	normalized := NormalizeAuthMetadata(metadata, provider)
	if !reflect.DeepEqual(metadata, normalized) {
		metadata = normalized
		if raw, errMarshal := json.Marshal(metadata); errMarshal == nil {
			if file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600); errOpen == nil {
				_, _ = file.Write(raw)
				_ = file.Close()
			}
		}
	}
	if provider == "antigravity" || provider == "gemini" {
		projectID := ""
		if pid, ok := metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
		if projectID == "" && shouldAttemptProjectIDFetch(metadata, time.Now()) {
			accessToken := extractAccessToken(metadata)
			// For gemini type, the stored access_token is likely expired (~1h lifetime).
			// Refresh it using the long-lived refresh_token before querying.
			if provider == "gemini" {
				if tokenMap, ok := metadata["token"].(map[string]any); ok {
					if refreshed, errRefresh := refreshGeminiAccessToken(tokenMap, http.DefaultClient); errRefresh == nil {
						accessToken = refreshed
					}
				}
			}
			if accessToken != "" {
				fetchedProjectID, errFetch := fetchAntigravityProjectID(context.Background(), accessToken, http.DefaultClient)
				if errFetch == nil && strings.TrimSpace(fetchedProjectID) != "" {
					metadata["project_id"] = strings.TrimSpace(fetchedProjectID)
					delete(metadata, projectIDFetchFailedAtKey)
					_ = persistAuthMetadata(path, metadata)
				} else if errFetch != nil {
					metadata[projectIDFetchFailedAtKey] = time.Now().UTC().Format(time.RFC3339)
					_ = persistAuthMetadata(path, metadata)
				}
			}
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	id := s.idFor(path, baseDir)
	auth := &cliproxyauth.Auth{
		ID:               id,
		TenantID:         cliproxyauth.TenantIDFromAuthID(id),
		Provider:         provider,
		Prefix:           metadataString(metadata, "prefix"),
		ProxyURL:         metadataString(metadata, "proxy_url", "proxy-url", "proxyUrl"),
		ProxyID:          metadataString(metadata, "proxy_id", "proxy-id", "proxyId"),
		FileName:         id,
		Label:            s.labelFor(metadata),
		Status:           cliproxyauth.StatusActive,
		Attributes:       buildFileAuthAttributes(path, metadata),
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	cliproxyauth.RestorePersistedDisabled(auth)
	return auth, nil
}

func shouldAttemptProjectIDFetch(metadata map[string]any, now time.Time) bool {
	if metadata == nil {
		return true
	}
	raw, ok := metadata[projectIDFetchFailedAtKey].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return true
	}
	failedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return now.Sub(failedAt) >= projectIDFetchCooldown
}

func persistAuthMetadata(path string, metadata map[string]any) error {
	raw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return errMarshal
	}
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if errOpen != nil {
		return errOpen
	}
	if _, errWrite := file.Write(raw); errWrite != nil {
		_ = file.Close()
		return errWrite
	}
	return file.Close()
}

// buildFileAuthAttributes keeps disk-loaded credentials aligned with OAuth login
// and management upload registration (auth_kind/base_url/using_api/email).
func buildFileAuthAttributes(path string, metadata map[string]any) map[string]string {
	attrs := map[string]string{"path": path}
	if email := metadataString(metadata, "email"); email != "" {
		attrs["email"] = email
	}
	if authKind := metadataString(metadata, "auth_kind", "authKind"); authKind != "" {
		attrs["auth_kind"] = authKind
	}
	if baseURL := metadataString(metadata, "base_url", "base-url", "baseUrl"); baseURL != "" {
		attrs["base_url"] = baseURL
	}
	if apiKey := metadataString(metadata, "api_key", "api-key", "apiKey"); apiKey != "" {
		attrs["api_key"] = apiKey
	}
	if raw, ok := metadata["using_api"]; ok {
		switch value := raw.(type) {
		case bool:
			if value {
				attrs["using_api"] = "true"
			} else {
				attrs["using_api"] = "false"
			}
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				attrs["using_api"] = trimmed
			}
		}
	}
	return attrs
}

func metadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if raw, ok := metadata[key].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

// InferAuthProvider returns the explicit provider type, or derives it for
// credential JSON shapes that are produced by supported OAuth flows.
func InferAuthProvider(metadata map[string]any) string {
	provider := metadataString(metadata, "type")
	if provider == "" && isCodexOAuthMetadata(metadata) {
		provider = "codex"
		if metadata != nil {
			metadata["type"] = provider
		}
	}
	if provider == "" {
		return "unknown"
	}
	return provider
}

func isCodexOAuthMetadata(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	hasAccount := metadataString(metadata, "chatgpt_account_id", "chatgptAccountID", "account_id") != ""
	hasToken := metadataString(metadata, "access_token", "id_token", "refresh_token", "session_token") != ""
	hasOpenAIClient := strings.HasPrefix(metadataString(metadata, "client_id"), "app_")
	hasCodexClaims := metadataString(metadata, "chatgpt_plan_type", "account_claims_email") != ""
	return hasToken && (hasAccount || hasOpenAIClient || hasCodexClaims)
}

// NormalizeAuthMetadata fills canonical fields expected by provider executors
// while preserving any source-specific metadata that may be useful later.
// Imported OAuth JSON (cross-tenant upload) must produce the same shape as a
// fresh login, otherwise executors and management quota calls fail silently.
func NormalizeAuthMetadata(metadata map[string]any, provider string) map[string]any {
	if len(metadata) == 0 {
		return metadata
	}
	normalized := maps.Clone(metadata)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		normalizeCodexAuthMetadata(normalized)
	case "xai", "x-ai", "grok":
		normalizeXAIAuthMetadata(normalized)
	case "claude", "anthropic":
		normalizeClaudeAuthMetadata(normalized)
	case "kimi":
		normalizeKimiAuthMetadata(normalized)
	case "gemini-cli", "gemini", "antigravity":
		normalizeGeminiFamilyAuthMetadata(normalized, provider)
	}
	return normalized
}

func normalizeXAIAuthMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	metadata["type"] = "xai"
	if metadataString(metadata, "auth_kind", "authKind") == "" {
		// OAuth credential files always carry refresh_token; API-key files use api_key.
		if metadataString(metadata, "refresh_token", "refreshToken") != "" ||
			metadataString(metadata, "access_token", "accessToken") != "" {
			metadata["auth_kind"] = "oauth"
		} else if metadataString(metadata, "api_key", "apiKey") != "" {
			metadata["auth_kind"] = "api_key"
		}
	} else if kind := metadataString(metadata, "auth_kind", "authKind"); kind != "" {
		metadata["auth_kind"] = kind
	}
	if metadataString(metadata, "base_url", "baseUrl") == "" {
		metadata["base_url"] = "https://api.x.ai/v1"
	} else if baseURL := metadataString(metadata, "base_url", "baseUrl"); baseURL != "" {
		metadata["base_url"] = baseURL
	}
	if metadataString(metadata, "token_endpoint", "tokenEndpoint") == "" {
		metadata["token_endpoint"] = "https://auth.x.ai/oauth2/token"
	}
	if email := metadataString(metadata, "email"); email != "" {
		metadata["email"] = email
	}
	if sub := metadataString(metadata, "sub", "subject", "user_id", "userId"); sub != "" {
		metadata["sub"] = sub
	}
	// Prefer JWT claims when local fields are missing or stale after export/import.
	normalizeXAIMetadataFromJWT(metadataString(metadata, "access_token", "accessToken"), metadata)
	normalizeXAIMetadataFromJWT(metadataString(metadata, "id_token", "idToken"), metadata)
	// OAuth credentials default to Grok Build (using_api=false) when unset.
	if _, ok := metadata["using_api"]; !ok {
		if strings.EqualFold(metadataString(metadata, "auth_kind"), "oauth") {
			metadata["using_api"] = false
		}
	}
}

func normalizeXAIMetadataFromJWT(token string, metadata map[string]any) {
	claims, ok := parseJWTClaimsMap(token)
	if !ok {
		return
	}
	if metadataString(metadata, "sub") == "" {
		if sub, ok := claims["sub"].(string); ok && strings.TrimSpace(sub) != "" {
			metadata["sub"] = strings.TrimSpace(sub)
		} else if principal, ok := claims["principal_id"].(string); ok && strings.TrimSpace(principal) != "" {
			metadata["sub"] = strings.TrimSpace(principal)
		}
	}
	if metadataString(metadata, "email") == "" {
		if email, ok := claims["email"].(string); ok && strings.TrimSpace(email) != "" {
			metadata["email"] = strings.TrimSpace(email)
		}
	}
	if metadataString(metadata, "expired") == "" {
		if exp, ok := jwtNumericClaim(claims, "exp"); ok && exp > 0 {
			metadata["expired"] = time.Unix(exp, 0).UTC().Format(time.RFC3339)
		}
	}
}

func normalizeClaudeAuthMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	if metadataString(metadata, "type") == "" {
		metadata["type"] = "claude"
	}
	if email := metadataString(metadata, "email"); email != "" {
		metadata["email"] = email
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" {
		if metadataString(metadata, "refresh_token", "refreshToken") != "" ||
			metadataString(metadata, "access_token", "accessToken") != "" {
			metadata["auth_kind"] = "oauth"
		}
	}
}

func normalizeKimiAuthMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	if metadataString(metadata, "type") == "" {
		metadata["type"] = "kimi"
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" {
		if metadataString(metadata, "refresh_token", "refreshToken") != "" ||
			metadataString(metadata, "access_token", "accessToken") != "" {
			metadata["auth_kind"] = "oauth"
		}
	}
}

func normalizeGeminiFamilyAuthMetadata(metadata map[string]any, provider string) {
	if metadata == nil {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "gemini"
	}
	if metadataString(metadata, "type") == "" {
		metadata["type"] = provider
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" {
		if metadataString(metadata, "refresh_token", "refreshToken") != "" ||
			hasNestedRefreshToken(metadata) {
			metadata["auth_kind"] = "oauth"
		}
	}
}

func hasNestedRefreshToken(metadata map[string]any) bool {
	tokenRaw, ok := metadata["token"].(map[string]any)
	if !ok || tokenRaw == nil {
		return false
	}
	return metadataString(tokenRaw, "refresh_token", "refreshToken") != ""
}

func syncRoutingMetadata(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}
	prefix := strings.TrimSpace(auth.Prefix)
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	proxyID := strings.TrimSpace(auth.ProxyID)
	if prefix == "" && proxyURL == "" && proxyID == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if prefix != "" {
		auth.Metadata["prefix"] = prefix
	}
	if proxyURL != "" {
		auth.Metadata["proxy_url"] = proxyURL
	}
	if proxyID != "" {
		auth.Metadata["proxy_id"] = proxyID
	}
}

func (s *FileTokenStore) idFor(path, baseDir string) string {
	if baseDir == "" {
		return path
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return path
	}
	return rel
}

func (s *FileTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if dir := s.baseDirSnapshot(); dir != "" {
			return filepath.Join(dir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, auth.ID), nil
}

func (s *FileTokenStore) labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["label"].(string); ok && v != "" {
		return v
	}
	if v, ok := metadata["email"].(string); ok && v != "" {
		return v
	}
	if project, ok := metadata["project_id"].(string); ok && project != "" {
		return project
	}
	return ""
}

func (s *FileTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}

func extractAccessToken(metadata map[string]any) string {
	if at, ok := metadata["access_token"].(string); ok {
		if v := strings.TrimSpace(at); v != "" {
			return v
		}
	}
	if tokenMap, ok := metadata["token"].(map[string]any); ok {
		if at, ok := tokenMap["access_token"].(string); ok {
			if v := strings.TrimSpace(at); v != "" {
				return v
			}
		}
	}
	return ""
}

func refreshGeminiAccessToken(tokenMap map[string]any, httpClient *http.Client) (string, error) {
	refreshToken, _ := tokenMap["refresh_token"].(string)
	clientID, _ := tokenMap["client_id"].(string)
	clientSecret, _ := tokenMap["client_secret"].(string)
	tokenURI, _ := tokenMap["token_uri"].(string)

	if refreshToken == "" || clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("missing refresh credentials")
	}
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}

	resp, err := httpClient.PostForm(tokenURI, data)
	if err != nil {
		return "", fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := util.ReadHTTPResponseBody("codex-device", resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed: status %d", resp.StatusCode)
	}

	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return "", fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}

	newAccessToken, _ := result["access_token"].(string)
	if newAccessToken == "" {
		return "", fmt.Errorf("no access_token in refresh response")
	}

	tokenMap["access_token"] = newAccessToken
	return newAccessToken, nil
}

// jsonEqual compares two JSON blobs by parsing them into Go objects and deep comparing.
func jsonEqual(a, b []byte) bool {
	var objA any
	var objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false
	}
	return deepEqualJSON(objA, objB)
}

func deepEqualJSON(a, b any) bool {
	switch valA := a.(type) {
	case map[string]any:
		valB, ok := b.(map[string]any)
		if !ok || len(valA) != len(valB) {
			return false
		}
		for key, subA := range valA {
			subB, ok1 := valB[key]
			if !ok1 || !deepEqualJSON(subA, subB) {
				return false
			}
		}
		return true
	case []any:
		sliceB, ok := b.([]any)
		if !ok || len(valA) != len(sliceB) {
			return false
		}
		for i := range valA {
			if !deepEqualJSON(valA[i], sliceB[i]) {
				return false
			}
		}
		return true
	case float64:
		valB, ok := b.(float64)
		if !ok {
			return false
		}
		return valA == valB
	case string:
		valB, ok := b.(string)
		if !ok {
			return false
		}
		return valA == valB
	case bool:
		valB, ok := b.(bool)
		if !ok {
			return false
		}
		return valA == valB
	case nil:
		return b == nil
	default:
		return false
	}
}
