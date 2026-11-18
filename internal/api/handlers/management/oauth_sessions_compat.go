package management

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/management/authfiles"
)

const (
	oauthSessionStatusCompleted  = "completed"
	oauthSessionStatusSuperseded = "superseded"
	oauthSupersededMessage       = "this login was replaced by another completed login of the same provider; start a new login"
	oauthCodeSessionExpired      = "oauth_login_expired"
	oauthCodeSessionSuperseded   = "oauth_login_superseded"
)

func GetOAuthSessionWithTenant(state string) (provider, tenantID, status string, ok bool) {
	provider, status, ok = GetOAuthSession(state)
	return provider, "", status, ok
}

func RegisterOAuthSessionForTenant(state, provider, tenantID string) {
	oauthSessions.Register(state, provider)
	if shared := sharedOAuthSessionStore(); shared != nil {
		shared.RegisterTenant(state, provider, tenantID)
	}
}

func CompleteOAuthSessionsByProviderForTenant(provider, tenantID string) int {
	count := oauthSessions.CompleteProvider(provider, oauthSessionSourceBuiltin)
	if shared := sharedOAuthSessionStore(); shared != nil {
		count += shared.CompleteProviderTenant(provider, tenantID)
	}
	return count
}

func WaitOAuthCallbackFile(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
	if shared := sharedOAuthSessionStore(); shared != nil {
		return shared.WaitCallback(authDir, provider, state, timeout, 500*time.Millisecond)
	}
	canonical, err := NormalizeOAuthCallbackProvider(provider)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(authDir) == "" {
		return nil, errors.New("auth dir is empty")
	}
	if err := ValidateOAuthState(state); err != nil {
		return nil, err
	}
	if tenantID := oauthSessionTenantID(state); tenantID != "" {
		authDir = authfiles.TenantAuthDir(authDir, tenantID)
	}
	path := filepath.Join(authDir, ".oauth-"+canonical+"-"+state+".oauth")
	deadline := time.Now().Add(timeout)
	for {
		if data, errRead := os.ReadFile(path); errRead == nil {
			var payload oauthCallbackFilePayload
			if errDecode := json.Unmarshal(data, &payload); errDecode != nil {
				return nil, errDecode
			}
			_ = os.Remove(path)
			return map[string]string{"code": payload.Code, "state": payload.State, "error": payload.Error}, nil
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return nil, os.ErrDeadlineExceeded
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func oauthSessionTenantID(_ string) string {
	return ""
}
