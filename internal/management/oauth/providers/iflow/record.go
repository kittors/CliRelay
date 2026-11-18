package iflow

import (
	"fmt"
	"strings"
	"time"

	internaliflow "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *internaliflow.IFlowTokenStorage, now time.Time) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	identifier := strings.TrimSpace(tokenStorage.Email)
	if identifier == "" {
		identifier = fmt.Sprintf("%d", now.UnixMilli())
		tokenStorage.Email = identifier
	}
	fileName := CredentialFileName(identifier)

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "iflow",
		FileName:   fileName,
		Storage:    tokenStorage,
		Metadata:   MetadataFromTokenStorage(tokenStorage, identifier),
		Attributes: AttributesFromTokenStorage(tokenStorage),
	}
}

func CookieRecordFromTokenStorage(tokenStorage *internaliflow.IFlowTokenStorage, now time.Time) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	email := strings.TrimSpace(tokenStorage.Email)
	if email == "" {
		return nil
	}

	fileStem := internaliflow.SanitizeIFlowFileName(email)
	if fileStem == "" {
		fileStem = fmt.Sprintf("iflow-%d", now.UnixMilli())
	} else {
		fileStem = fmt.Sprintf("iflow-%s", fileStem)
	}
	fileName := fmt.Sprintf("%s-%d.json", fileStem, now.Unix())
	tokenStorage.Email = email

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "iflow",
		FileName:   fileName,
		Storage:    tokenStorage,
		Metadata:   CookieMetadataFromTokenStorage(tokenStorage),
		Attributes: AttributesFromTokenStorage(tokenStorage),
	}
}

func MetadataFromTokenStorage(tokenStorage *internaliflow.IFlowTokenStorage, identifier string) map[string]any {
	metadata := map[string]any{
		"email": strings.TrimSpace(identifier),
	}
	if tokenStorage == nil {
		return metadata
	}
	metadata["api_key"] = tokenStorage.APIKey
	return metadata
}

func CookieMetadataFromTokenStorage(tokenStorage *internaliflow.IFlowTokenStorage) map[string]any {
	if tokenStorage == nil {
		return map[string]any{}
	}
	return map[string]any{
		"email":        strings.TrimSpace(tokenStorage.Email),
		"api_key":      tokenStorage.APIKey,
		"expired":      tokenStorage.Expire,
		"cookie":       tokenStorage.Cookie,
		"type":         tokenStorage.Type,
		"last_refresh": tokenStorage.LastRefresh,
	}
}

func AttributesFromTokenStorage(tokenStorage *internaliflow.IFlowTokenStorage) map[string]string {
	if tokenStorage == nil {
		return map[string]string{}
	}
	return map[string]string{"api_key": tokenStorage.APIKey}
}

func CredentialFileName(identifier string) string {
	return fmt.Sprintf("iflow-%s.json", strings.TrimSpace(identifier))
}
