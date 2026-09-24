package config

import "testing"

// The shipped example must parse and land on the documented defaults; an example
// that silently fails to load is how an operator ends up with a config they
// think is active.
func TestExampleConfigLoadsAccountStatusRefresh(t *testing.T) {
	cfg, err := LoadConfig("../../config.example.yaml")
	if err != nil {
		t.Fatalf("load config.example.yaml: %v", err)
	}
	refresh := cfg.AccountStatusRefresh
	if !refresh.Enabled {
		t.Fatal("example config must ship the background quota probe enabled")
	}
	if refresh.IntervalMinutes != 15 || refresh.StartupDelaySeconds != 60 {
		t.Fatalf("example refresh = %+v, want interval 15 / delay 60", refresh)
	}
}

// Fresh installs copy this file verbatim (clirelay-init and the git/object/postgres
// store bootstraps), so any client key left active here is a working credential on
// every such deployment. Shipping none is only safe while allow-unauthenticated
// stays off: with it on, "no keys" means an open client API.
func TestExampleConfigShipsNoActiveClientAPIKeys(t *testing.T) {
	cfg, err := LoadConfig("../../config.example.yaml")
	if err != nil {
		t.Fatalf("load config.example.yaml: %v", err)
	}
	if len(cfg.APIKeys) != 0 || len(cfg.APIKeyEntries) != 0 {
		t.Fatalf("example config ships active client API keys: api-keys=%q api-key-entries=%d", cfg.APIKeys, len(cfg.APIKeyEntries))
	}
	if cfg.AllowUnauthenticated {
		t.Fatal("example config must keep allow-unauthenticated off")
	}
}
