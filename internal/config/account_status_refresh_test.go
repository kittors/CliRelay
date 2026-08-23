package config

import "testing"

func TestSanitizeAccountStatusRefreshDefaultsAndFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		in           AccountStatusRefreshConfig
		wantInterval int
		wantDelay    int
	}{
		{"unset takes defaults", AccountStatusRefreshConfig{Enabled: true}, 15, 60},
		{"explicit values kept", AccountStatusRefreshConfig{Enabled: true, IntervalMinutes: 30, StartupDelaySeconds: 5}, 30, 5},
		// A one-minute interval would probe every billing endpoint 60x an hour
		// per account for readings the snapshot heartbeat would discard anyway.
		{"too-short interval floors", AccountStatusRefreshConfig{Enabled: true, IntervalMinutes: 1}, 5, 60},
		// Unset and negative alike take the default: firing one probe per
		// credential in the same instant as boot is a burst nobody asked for.
		{"unset delay takes the default", AccountStatusRefreshConfig{Enabled: true, IntervalMinutes: 15, StartupDelaySeconds: 0}, 15, 60},
		{"negative delay takes the default", AccountStatusRefreshConfig{Enabled: true, IntervalMinutes: 15, StartupDelaySeconds: -1}, 15, 60},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{AccountStatusRefresh: tc.in}
			cfg.SanitizeAccountStatusRefresh()
			if cfg.AccountStatusRefresh.IntervalMinutes != tc.wantInterval {
				t.Fatalf("interval=%d, want %d", cfg.AccountStatusRefresh.IntervalMinutes, tc.wantInterval)
			}
			if cfg.AccountStatusRefresh.StartupDelaySeconds != tc.wantDelay {
				t.Fatalf("startup delay=%d, want %d", cfg.AccountStatusRefresh.StartupDelaySeconds, tc.wantDelay)
			}
		})
	}
}

// Sanitizing must not switch the probe on for an operator who turned it off.
func TestSanitizeAccountStatusRefreshKeepsDisabled(t *testing.T) {
	t.Parallel()

	cfg := &Config{AccountStatusRefresh: AccountStatusRefreshConfig{Enabled: false}}
	cfg.SanitizeAccountStatusRefresh()
	if cfg.AccountStatusRefresh.Enabled {
		t.Fatal("sanitize re-enabled a disabled probe")
	}
}

func TestDefaultAccountStatusRefreshIsOn(t *testing.T) {
	t.Parallel()

	if !defaultAccountStatusRefreshConfig().Enabled {
		t.Fatal("background quota refresh must default to on; otherwise xAI accounts spent outside the proxy stay stale")
	}
}
