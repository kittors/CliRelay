package usagelogs

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type Service struct {
	tenantID    string
	cfg         *config.Config
	authManager *coreauth.Manager
}

func New(cfg *config.Config, authManager *coreauth.Manager) *Service {
	return NewForTenant("", cfg, authManager)
}

func NewForTenant(tenantID string, cfg *config.Config, authManager *coreauth.Manager) *Service {
	return &Service{tenantID: tenantID, cfg: cfg, authManager: authManager}
}

func errorPayload(code, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
}

func internalErrorPayload(action string, err error) map[string]any {
	log.WithField("action", action).WithError(err).Error("management usagelogs internal error")
	return errorPayload("internal_error", fmt.Sprintf("%s失败，服务端内部处理异常，请稍后重试或查看服务端日志", action))
}
