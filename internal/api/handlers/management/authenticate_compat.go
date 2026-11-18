package management

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"golang.org/x/crypto/bcrypt"
)

// AuthenticateManagementKey provides the non-HTTP authentication surface used
// by the Redis protocol. It follows the same credential precedence as the HTTP
// middleware while returning a status and plain diagnostic string.
func (h *Handler) AuthenticateManagementKey(_ string, localClient bool, provided string) (bool, int, string) {
	if h == nil {
		return false, http.StatusServiceUnavailable, "management handler unavailable"
	}
	cfg := h.configSnapshot()
	allowRemote := false
	secretHash := ""
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = strings.TrimSpace(cfg.RemoteManagement.SecretKey)
	}
	if h.allowRemoteOverride {
		allowRemote = true
	}
	if !localClient && !allowRemote {
		return false, http.StatusForbidden, "remote management disabled"
	}

	provided = strings.TrimSpace(provided)
	if provided == "" {
		return false, http.StatusUnauthorized, "missing management key"
	}
	if localClient && h.localPassword != "" && util.ConstantTimeStringEqual(provided, h.localPassword) {
		return true, http.StatusOK, ""
	}
	if h.envSecret != "" && util.ConstantTimeStringEqual(provided, h.envSecret) {
		return true, http.StatusOK, ""
	}
	if secretHash != "" {
		if h.authCache.verify(secretHash, provided) || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) == nil {
			return true, http.StatusOK, ""
		}
	}
	return false, http.StatusUnauthorized, "invalid management key"
}
