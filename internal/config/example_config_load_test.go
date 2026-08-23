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
