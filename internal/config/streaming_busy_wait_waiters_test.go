package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streaming.busy-wait-max-waiters is a process-level knob read from config.yaml,
// like streaming.bootstrap-retries: unset, 0 and negative values all take the
// default (none of them means unlimited), and a set value has to survive a
// load, a management save that rewrites config.yaml and the management JSON.
func TestStreamingBusyWaitMaxWaiters(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantWaiters int
		wantWritten string // text the saved file must hold; empty means the key must be absent
	}{
		{"unset", "port: 8317\nstreaming:\n  keepalive-seconds: 20\n", DefaultBusyWaitMaxWaiters, ""},
		{"zero", "port: 8317\nstreaming:\n  busy-wait-max-waiters: 0\n", DefaultBusyWaitMaxWaiters, "busy-wait-max-waiters: 0"},
		{"negative", "port: 8317\nstreaming:\n  busy-wait-max-waiters: -5\n", DefaultBusyWaitMaxWaiters, "busy-wait-max-waiters: -5"},
		{"explicit", "port: 8317\nstreaming:\n  busy-wait-max-waiters: 12\n", 12, "busy-wait-max-waiters: 12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Streaming.BusyWaiters(); got != tc.wantWaiters {
				t.Fatalf("effective cap after load = %d, want %d", got, tc.wantWaiters)
			}

			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWritten == "" && strings.Contains(string(saved), "busy-wait-max-waiters") {
				t.Fatalf("an unset key was written back:\n%s", saved)
			}
			if tc.wantWritten != "" && !strings.Contains(string(saved), tc.wantWritten) {
				t.Fatalf("saved file lost %q:\n%s", tc.wantWritten, saved)
			}
			restored, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := restored.Streaming.BusyWaiters(); got != tc.wantWaiters {
				t.Fatalf("effective cap after save and reload = %d, want %d", got, tc.wantWaiters)
			}

			encoded, err := json.Marshal(cfg.Streaming)
			if err != nil {
				t.Fatal(err)
			}
			var decoded StreamingConfig
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := decoded.BusyWaiters(); got != tc.wantWaiters {
				t.Fatalf("effective cap after a JSON round trip = %d, want %d", got, tc.wantWaiters)
			}
		})
	}
}

// The default stays a fraction of what the reverse proxy lets through: nginx's
// max_conns is 400 in the production deployment, and every waiter holds one of
// those connections idle.
func TestDefaultBusyWaitMaxWaitersLeavesRoomForOtherTraffic(t *testing.T) {
	if DefaultBusyWaitMaxWaiters <= 0 || DefaultBusyWaitMaxWaiters > 400/4 {
		t.Fatalf("DefaultBusyWaitMaxWaiters = %d, want a positive cap of at most a quarter of nginx max_conns (400)", DefaultBusyWaitMaxWaiters)
	}
}
