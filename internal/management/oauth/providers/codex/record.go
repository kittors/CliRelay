package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *internalcodex.CodexTokenStorage) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}

	planType, accountHash := planAndAccountHashFromIDToken(tokenStorage.IDToken)
	fileName := internalcodex.CredentialFileName(tokenStorage.Email, planType, accountHash, true)

	return &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: MetadataFromTokenStorage(tokenStorage, planType),
	}
}

func MetadataFromTokenStorage(tokenStorage *internalcodex.CodexTokenStorage, planType string) map[string]any {
	metadata := map[string]any{
		"plan_type": strings.ToLower(strings.TrimSpace(planType)),
	}
	if tokenStorage == nil {
		return metadata
	}
	metadata["email"] = tokenStorage.Email
	metadata["account_id"] = tokenStorage.AccountID
	if claims, _ := internalcodex.ParseJWTToken(tokenStorage.IDToken); claims != nil {
		if userID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID); userID != "" {
			metadata["chatgpt_user_id"] = userID
		}
	}
	return metadata
}

func planAndAccountHashFromIDToken(idToken string) (string, string) {
	claims, _ := internalcodex.ParseJWTToken(idToken)
	if claims == nil {
		return "", ""
	}

	planType := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
	accountID := claims.GetAccountID()
	if accountID == "" {
		return planType, ""
	}

	digest := sha256.Sum256([]byte(accountID))
	return planType, hex.EncodeToString(digest[:])[:8]
}
