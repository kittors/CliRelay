package config

import (
	"os"
	"strings"
)

// DBResilienceConfig tunes how the gateway rides out a short database outage
// (a restart, or a primary failover of up to a minute or two) without failing
// requests or losing usage records. Every field is zero-means-default, so an
// absent block gives the recommended behaviour.
type DBResilienceConfig struct {
	// UsageSpoolDir holds request log writes the database could not take; they
	// are replayed in order once it accepts writes again, including after a
	// restart. It must be on persistent storage. Empty picks a directory next
	// to the auth directory (see the resolver in internal/cmd).
	UsageSpoolDir string `yaml:"usage-spool-dir,omitempty" json:"usage-spool-dir,omitempty"`
	// UsageSpoolMaxSizeMB caps the spool on disk. When it is full the oldest
	// records are dropped, with an error log, to make room. Default 1024; a
	// negative value switches spooling off and restores the old behaviour of
	// dropping a record the database refuses.
	UsageSpoolMaxSizeMB int `yaml:"usage-spool-max-size-mb,omitempty" json:"usage-spool-max-size-mb,omitempty"`
}

const (
	// EnvUsageSpoolDir overrides db-resilience.usage-spool-dir, for container
	// and systemd deployments that mount the persistent volume elsewhere.
	EnvUsageSpoolDir = "CLIRELAY_USAGE_SPOOL_DIR"
)

// UsageSpoolDirOverride returns the configured spool directory, with the
// environment variable taking precedence. Empty means "use the default".
func (c DBResilienceConfig) UsageSpoolDirOverride() string {
	if v := strings.TrimSpace(os.Getenv(EnvUsageSpoolDir)); v != "" {
		return v
	}
	return strings.TrimSpace(c.UsageSpoolDir)
}
