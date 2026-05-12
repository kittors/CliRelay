package cliproxy

import (
	"context"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type ProxyBanNotifier interface {
	OnAuthRevoked(auth *coreauth.Auth)
}

type compositeHook struct {
	coreauth.NoopHook
	mu        sync.RWMutex
	notifiers []ProxyBanNotifier
}

func (h *compositeHook) OnAuthUpdated(ctx context.Context, auth *coreauth.Auth) {
	if auth == nil || auth.Status != coreauth.StatusRevoked {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, n := range h.notifiers {
		n.OnAuthRevoked(auth)
	}
}

func (h *compositeHook) AddNotifier(n ProxyBanNotifier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifiers = append(h.notifiers, n)
}
