package config

import (
	"os"
	"strconv"
	"strings"
)

// ClusterConfig controls multi-instance deployment: several CliRelay
// processes sharing one PostgreSQL primary. Leaving it disabled keeps the
// single-node behaviour unchanged.
type ClusterConfig struct {
	// Enabled turns on cluster coordination: credentials and settings are
	// shared through PostgreSQL, periodic maintenance runs on one elected
	// node, and changes are broadcast to the other nodes.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// NodeID uniquely names this process among the nodes sharing a
	// database. Empty means the host name.
	NodeID string `yaml:"node-id,omitempty" json:"node-id,omitempty"`
	// Advertise is an optional address shown in the cluster status view,
	// for operators only.
	Advertise string `yaml:"advertise,omitempty" json:"advertise,omitempty"`
	// Redis is the Redis shared by all nodes for cluster-wide rate limits,
	// concurrency slots and session affinity. It is separate from the
	// top-level redis block, which stays node-local. Empty Addr means those
	// counters stay per node and limits are divided by the active node count.
	Redis ClusterRedisConfig `yaml:"redis,omitempty" json:"redis,omitempty"`
}

// ClusterRedisConfig describes the shared cluster Redis.
type ClusterRedisConfig struct {
	Addr     string `yaml:"addr,omitempty" json:"addr,omitempty"`
	Password string `yaml:"password,omitempty" json:"-"`
	DB       int    `yaml:"db,omitempty" json:"db,omitempty"`
	// TLS settings. The cluster Redis is reached over the public network, so
	// production deployments set all three and use mutual TLS.
	TLSCAFile   string `yaml:"tls-ca-file,omitempty" json:"tls-ca-file,omitempty"`
	TLSCertFile string `yaml:"tls-cert-file,omitempty" json:"tls-cert-file,omitempty"`
	TLSKeyFile  string `yaml:"tls-key-file,omitempty" json:"tls-key-file,omitempty"`
	// TLSServerName overrides the name checked against the server
	// certificate; defaults to the host part of Addr.
	TLSServerName string `yaml:"tls-server-name,omitempty" json:"tls-server-name,omitempty"`
}

// Environment overrides for the cluster block, so secrets and per-node
// values can live in the systemd EnvironmentFile instead of config.yaml.
const (
	EnvClusterEnabled       = "CLIRELAY_CLUSTER_ENABLED"
	EnvClusterNodeID        = "CLIRELAY_CLUSTER_NODE_ID"
	EnvClusterAdvertise     = "CLIRELAY_CLUSTER_ADVERTISE"
	EnvClusterRedisAddr     = "CLIRELAY_CLUSTER_REDIS_ADDR"
	EnvClusterRedisPassword = "CLIRELAY_CLUSTER_REDIS_PASSWORD"
	EnvClusterRedisDB       = "CLIRELAY_CLUSTER_REDIS_DB"
	EnvClusterRedisTLSCA    = "CLIRELAY_CLUSTER_REDIS_TLS_CA_FILE"
	EnvClusterRedisTLSCert  = "CLIRELAY_CLUSTER_REDIS_TLS_CERT_FILE"
	EnvClusterRedisTLSKey   = "CLIRELAY_CLUSTER_REDIS_TLS_KEY_FILE"
	EnvClusterRedisTLSName  = "CLIRELAY_CLUSTER_REDIS_TLS_SERVER_NAME"
)

func (cfg *Config) applyClusterEnvOverrides() {
	if raw := strings.TrimSpace(os.Getenv(EnvClusterEnabled)); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			cfg.Cluster.Enabled = enabled
		}
	}
	setString := func(dst *string, key string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*dst = v
		}
	}
	setString(&cfg.Cluster.NodeID, EnvClusterNodeID)
	setString(&cfg.Cluster.Advertise, EnvClusterAdvertise)
	setString(&cfg.Cluster.Redis.Addr, EnvClusterRedisAddr)
	if v := os.Getenv(EnvClusterRedisPassword); v != "" {
		cfg.Cluster.Redis.Password = v
	}
	if raw := strings.TrimSpace(os.Getenv(EnvClusterRedisDB)); raw != "" {
		if db, err := strconv.Atoi(raw); err == nil && db >= 0 {
			cfg.Cluster.Redis.DB = db
		}
	}
	setString(&cfg.Cluster.Redis.TLSCAFile, EnvClusterRedisTLSCA)
	setString(&cfg.Cluster.Redis.TLSCertFile, EnvClusterRedisTLSCert)
	setString(&cfg.Cluster.Redis.TLSKeyFile, EnvClusterRedisTLSKey)
	setString(&cfg.Cluster.Redis.TLSServerName, EnvClusterRedisTLSName)
}
