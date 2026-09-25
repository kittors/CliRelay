package dnswatch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readDeployFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "cluster", "dnswatch", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func editedExampleConfig(t *testing.T) *Config {
	t.Helper()
	edited := strings.NewReplacer(
		"REPLACE_WITH_ZONE_ID", testZoneID,
		"REPLACE_WITH_APP_1_IPV4", "198.51.100.10",
		"REPLACE_WITH_APP_2_IPV4", "198.51.100.20",
	).Replace(readDeployFile(t, "dnswatch.example.yaml"))
	cfg, err := ParseConfig([]byte(edited))
	if err != nil {
		t.Fatalf("the example config with placeholders filled in must parse: %v", err)
	}
	return cfg
}

func TestExampleConfigRefusesUnfilledPlaceholders(t *testing.T) {
	_, err := ParseConfig([]byte(readDeployFile(t, "dnswatch.example.yaml")))
	if err == nil {
		t.Fatal("an unedited copy of the example must fail validation")
	}
	for _, fragment := range []string{"zone_id", "REPLACE_WITH_APP_1_IPV4", "REPLACE_WITH_APP_2_IPV4"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("validation error should point at %s: %v", fragment, err)
		}
	}
}

func TestExampleConfigDocumentsTheDefaults(t *testing.T) {
	cfg := editedExampleConfig(t)
	if !slices.Equal(cfg.Records, []string{"relay.07230805.xyz", "code.07230805.xyz"}) || cfg.Probe.Host != "relay.07230805.xyz" {
		t.Fatalf("unexpected records/host: %v %s", cfg.Records, cfg.Probe.Host)
	}
	p := cfg.Probe
	if cfg.TTL != DefaultTTL || p.Path != DefaultProbePath || p.Interval != DefaultProbeInterval ||
		p.Timeout != DefaultProbeTimeout || p.FailThreshold != DefaultFailThreshold ||
		p.RecoverThreshold != DefaultRecoverThreshold || !slices.Equal(p.ExpectStatus, defaultExpectStatus) ||
		cfg.MinChangeInterval != DefaultMinChangeInterval || cfg.SummaryInterval != DefaultSummaryInterval {
		t.Fatalf("the example's values drifted from the defaults it documents: %+v", cfg)
	}
	if !cfg.DryRun {
		t.Fatal("the example should start in dry_run so a first install cannot change DNS")
	}
	if cfg.StatusListen != "127.0.0.1:9109" || cfg.AlertWebhook != "" {
		t.Fatalf("unexpected optional settings: status_listen=%q", cfg.StatusListen)
	}
}

func TestSystemdUnitMatchesExampleConfig(t *testing.T) {
	unit := readDeployFile(t, "clirelay-dnswatch.service")
	for _, directive := range []string{
		"Restart=always",
		"DynamicUser=yes",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"CapabilityBoundingSet=\n",
		"RestartPreventExitStatus=2",
		"LoadCredential=dnswatch.yaml:/etc/clirelay-dnswatch/dnswatch.yaml",
		"ExecStart=/usr/local/bin/clirelay-dnswatch -config %d/dnswatch.yaml",
		"Environment=GOMEMLIMIT=",
	} {
		if !strings.Contains(unit, directive) {
			t.Errorf("unit lacks %q", directive)
		}
	}

	cfg := editedExampleConfig(t)
	// The config names its token file relative to itself; under the unit that
	// is the credentials directory, so the unit must load it by that name.
	if want := "LoadCredential=" + cfg.Cloudflare.APITokenFile + ":/etc/clirelay-dnswatch/"; !strings.Contains(unit, want) {
		t.Errorf("unit does not load the token file the example config names (%q)", want)
	}
	// The hold file has to stay somewhere operators can create it, outside
	// the read-only credentials directory.
	if !strings.HasPrefix(cfg.HoldFile, "/etc/clirelay-dnswatch/") {
		t.Errorf("hold_file %q should live in /etc/clirelay-dnswatch", cfg.HoldFile)
	}
}

func TestExampleConfigEnablesTheEgressProbe(t *testing.T) {
	if got := editedExampleConfig(t).Probe.EgressPath; got != "/readyz/egress" {
		t.Fatalf("the example should probe /readyz/egress, got %q", got)
	}
}
