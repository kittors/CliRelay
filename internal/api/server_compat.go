package api

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// getConfig is the read-only compatibility accessor used by auxiliary
// protocols such as the Redis usage stream.
func (s *Server) getConfig() *config.Config {
	if s == nil {
		return nil
	}
	return s.liveConfig(nil)
}
