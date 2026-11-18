package management

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/management/authfiles"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

func (h *Handler) authFileRepository() authfiles.Repository {
	if h == nil {
		return authfiles.Repository{}
	}
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	baseDir := ""
	if h.cfg != nil {
		baseDir = h.cfg.AuthDir
	}
	return authfiles.Repository{
		Store:        store,
		BaseDir:      baseDir,
		PostAuthHook: h.postAuthHook,
	}
}
