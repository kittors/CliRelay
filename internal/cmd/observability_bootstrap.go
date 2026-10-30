package cmd

import (
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// initializeObservability feeds upstream usage into the metrics and attaches
// the settings store. Collection does not depend on the store: counters are
// kept in memory whether or not the endpoint is on, so turning it on shows
// everything since the process started.
func initializeObservability() {
	observability.RegisterUsagePlugin()
	if usage.RuntimeDB() == nil {
		return
	}
	// An unreadable row leaves the endpoint off, the safe side for a page that
	// exposes the whole platform's traffic.
	if err := observability.Default().SetPersister(settingsstore.ObservabilitySettings{}); err != nil {
		log.WithError(err).Warn("observability: stored metrics settings are unreadable; the metrics endpoint stays off")
	}
}
