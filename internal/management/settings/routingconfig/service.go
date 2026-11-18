package routingconfig

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func Get() *config.RoutingConfig {
	return usage.GetRoutingConfig()
}

func Upsert(cfg config.RoutingConfig) error {
	return usage.UpsertRoutingConfig(cfg)
}
