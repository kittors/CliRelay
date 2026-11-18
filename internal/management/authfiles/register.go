package authfiles

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type Registrar struct {
	Manager  *coreauth.Manager
	AuthDir  string
	TenantID string
	Now      time.Time
}

func (r Registrar) RegisterFile(ctx context.Context, path string, data []byte) error {
	if r.Manager == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read auth file: %w", err)
		}
	}
	metadata, provider, err := r.normalizeFile(path, data)
	if err != nil {
		return err
	}
	authID := AuthIDForPath(r.AuthDir, path)
	if authID == "" {
		authID = path
	}
	opts := RecordOptions{
		AuthDir:  r.AuthDir,
		Path:     path,
		Provider: provider,
		Metadata: metadata,
		Now:      r.Now,
	}
	opts.TenantID = NormalizeTenantID(r.TenantID)
	if existing, ok := r.Manager.GetByID(authID); ok {
		opts.Existing = existing
		auth := BuildRecord(opts)
		_, err := r.Manager.Update(ctx, auth)
		return err
	}
	auth := BuildRecord(opts)
	_, err = r.Manager.Register(ctx, auth)
	return err
}

func (r Registrar) normalizeFile(path string, data []byte) (map[string]any, string, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, "", fmt.Errorf("invalid auth file: %w", err)
	}
	provider := sdkAuth.InferAuthProvider(metadata)
	normalized := sdkAuth.NormalizeAuthMetadata(metadata, provider)
	if reflect.DeepEqual(metadata, normalized) {
		return metadata, provider, nil
	}
	normalizedData, errMarshal := json.Marshal(normalized)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("failed to normalize auth file: %w", errMarshal)
	}
	if errWrite := os.WriteFile(path, normalizedData, 0o600); errWrite != nil {
		return nil, "", fmt.Errorf("failed to write normalized auth file: %w", errWrite)
	}
	return normalized, provider, nil
}
