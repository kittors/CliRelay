package qwen

import (
	"fmt"
	"strings"
	"time"

	internalqwen "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qwen"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *internalqwen.QwenTokenStorage, now time.Time) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	identifier := fmt.Sprintf("%d", now.UnixMilli())
	tokenStorage.Email = identifier
	fileName := CredentialFileName(identifier)

	return &coreauth.Auth{
		ID:       fileName,
		Provider: "qwen",
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: MetadataFromTokenStorage(tokenStorage),
	}
}

func MetadataFromTokenStorage(tokenStorage *internalqwen.QwenTokenStorage) map[string]any {
	if tokenStorage == nil {
		return map[string]any{}
	}
	return map[string]any{"email": tokenStorage.Email}
}

func CredentialFileName(identifier string) string {
	return fmt.Sprintf("qwen-%s.json", strings.TrimSpace(identifier))
}
