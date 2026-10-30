package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// metricsHandler points the exporter's scrape-time state at this server and
// returns the /metrics handler. The auth manager is looked up on every scrape
// because a config reload can swap it.
func (s *Server) metricsHandler() gin.HandlerFunc {
	exporter := observability.Default()
	exporter.SetSources(observability.Sources{
		Auths: func() []*auth.Auth {
			if manager := s.coreManager(); manager != nil {
				return manager.List()
			}
			return nil
		},
		InFlight: s.inFlightRequests.Load,
	})
	return exporter.Handler()
}
