package management

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// alertDispatcher returns the one dispatcher the alert service and the
// evaluation loop share, so both reuse its connections.
func (h *Handler) alertDispatcher() *alerting.Dispatcher {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alertSender == nil {
		h.alertSender = alerting.NewDispatcher()
	}
	return h.alertSender
}

// alerts returns the service behind the /alerts routes. Tests replace
// alertService to point it at a store of their own.
func (h *Handler) alerts() *alerting.Service {
	sender := h.alertDispatcher()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alertService == nil {
		h.alertService = alerting.NewService(settingsstore.AlertSettings{}, sender)
	}
	return h.alertService
}

// StartAlertEngine starts (or restarts) the background evaluation of alert
// rules. Called from server startup, like the account status scheduler: it has
// to run for tenants nobody is looking at.
//
// It costs one settings query a minute while no tenant has alerting on, runs on
// the cluster leader only, and reads stored data: nothing it does is on the
// request path.
func (h *Handler) StartAlertEngine() {
	if h == nil {
		return
	}
	sender := h.alertDispatcher()
	engine := alerting.NewEngine(alerting.EngineOptions{
		Store:    settingsstore.AlertSettings{},
		Sources:  alertSources{authManager: h.currentAuthManager},
		Sender:   sender,
		IsLeader: func() bool { return cluster.Default().IsLeader() },
		Interval: alerting.DefaultInterval, StartDelay: alerting.DefaultStartDelay,
	})
	h.mu.Lock()
	previous := h.alertEngine
	h.alertEngine = engine
	h.mu.Unlock()
	if previous != nil {
		previous.Stop()
	}
	engine.Start()
}

func (h *Handler) stopAlertEngine() {
	if h == nil {
		return
	}
	h.mu.Lock()
	engine := h.alertEngine
	h.alertEngine = nil
	h.mu.Unlock()
	if engine != nil {
		engine.Stop()
	}
}

// currentAuthManager is read per evaluation round rather than captured once:
// SetAuthManager can replace the manager after the engine started.
func (h *Handler) currentAuthManager() *coreauth.Manager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authManager
}
