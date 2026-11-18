package geminicli

import (
	geminiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *geminiauth.GeminiTokenStorage) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	fileName := geminiauth.CredentialFileName(tokenStorage.Email, tokenStorage.ProjectID, true)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "gemini",
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: MetadataFromTokenStorage(tokenStorage),
	}
}

func MetadataFromTokenStorage(tokenStorage *geminiauth.GeminiTokenStorage) map[string]any {
	if tokenStorage == nil {
		return map[string]any{}
	}
	return map[string]any{
		"email":      tokenStorage.Email,
		"project_id": tokenStorage.ProjectID,
		"auto":       tokenStorage.Auto,
		"checked":    tokenStorage.Checked,
	}
}
