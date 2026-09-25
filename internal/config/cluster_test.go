package config

import "testing"

func TestClusterEnvOverrides(t *testing.T) {
	t.Setenv(EnvClusterEnabled, "true")
	t.Setenv(EnvClusterNodeID, "n2")
	t.Setenv(EnvClusterRedisAddr, "10.0.0.3:6380")
	t.Setenv(EnvClusterRedisDB, "2")
	cfg := &Config{}
	cfg.ApplyEnvOverrides()
	if !cfg.Cluster.Enabled || cfg.Cluster.NodeID != "n2" || cfg.Cluster.Redis.Addr != "10.0.0.3:6380" || cfg.Cluster.Redis.DB != 2 {
		t.Fatalf("cluster env overrides not applied: %+v", cfg.Cluster)
	}
}

func TestClusterDisabledByDefault(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyEnvOverrides()
	if cfg.Cluster.Enabled {
		t.Fatalf("cluster mode must be opt-in")
	}
}
