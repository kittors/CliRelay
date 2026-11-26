package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *internalcodex.CodexTokenStorage) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}

	planType, accountHash := planAndAccountHashFromIDToken(tokenStorage.IDToken)
	fileName := internalcodex.CredentialFileName(tokenStorage.Email, planType, accountHash, true)

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: MetadataFromTokenStorage(tokenStorage, planType),
	}
	if ts := strings.TrimSpace(tokenStorage.LastRefresh); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			record.LastRefreshedAt = parsed.UTC()
		}
	}
	return record
}

func MetadataFromTokenStorage(tokenStorage *internalcodex.CodexTokenStorage, planType string) map[string]any {
	metadata := map[string]any{
		"type":      "codex",
		"plan_type": strings.ToLower(strings.TrimSpace(planType)),
	}
	if tokenStorage == nil {
		return metadata
	}
	metadata["email"] = tokenStorage.Email
	metadata["account_id"] = tokenStorage.AccountID
	// Mirror the token fields into Metadata immediately so in-memory consumers
	// (reauth replacement detection, status representative scoring, refresh
	// guards) see the same shape before the mirror reload arrives.
	if v := strings.TrimSpace(tokenStorage.IDToken); v != "" {
		metadata["id_token"] = v
	}
	if v := strings.TrimSpace(tokenStorage.AccessToken); v != "" {
		metadata["access_token"] = v
	}
	if v := strings.TrimSpace(tokenStorage.RefreshToken); v != "" {
		metadata["refresh_token"] = v
	}
	if v := strings.TrimSpace(tokenStorage.Expire); v != "" {
		metadata["expired"] = v
	}
	if v := strings.TrimSpace(tokenStorage.LastRefresh); v != "" {
		metadata["last_refresh"] = v
	}
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
