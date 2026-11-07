package management

import (
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// ensureObservabilityRuntime attaches the settings store to the metrics
// exporter whenever a database is available, like ensureIPAccessRuntime: a
// caller that assembles the management API itself (the route smoke tests, an
// SDK embedding) must get working settings endpoints, not 503.
func ensureObservabilityRuntime() {
	if usage.RuntimeDB() == nil {
		return
	}
	if err := observability.Default().SetPersister(settingsstore.ObservabilitySettings{}); err != nil {
		log.WithError(err).Warn("observability: stored metrics settings are unreadable; the metrics endpoint stays off")
	}
}
