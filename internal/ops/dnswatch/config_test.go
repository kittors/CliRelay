package dnswatch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalConfigYAML = `
cloudflare:
  api_token_file: cf-token
  zone_id: "0123456789abcdef0123456789abcdef"
records:
  - Relay.DNSWatch.test.
  - code.dnswatch.test
nodes:
  - name: node-a
    ip: 203.0.113.11
  - name: node-b
    ip: 203.0.113.12
`

func TestParseConfigAppliesDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(minimalConfigYAML))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if want := []string{"relay.dnswatch.test", "code.dnswatch.test"}; !slices.Equal(cfg.Records, want) {
		t.Fatalf("records = %v, want %v", cfg.Records, want)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"api_base_url", cfg.Cloudflare.APIBaseURL, DefaultAPIBaseURL},
		{"ttl", cfg.TTL, 60},
		{"probe.path", cfg.Probe.Path, "/readyz"},
		{"probe.host", cfg.Probe.Host, "relay.dnswatch.test"},
		{"probe.interval", cfg.Probe.Interval, 10 * time.Second},
		{"probe.timeout", cfg.Probe.Timeout, 5 * time.Second},
		{"probe.fail_threshold", cfg.Probe.FailThreshold, 3},
		{"probe.recover_threshold", cfg.Probe.RecoverThreshold, 3},
		{"min_change_interval", cfg.MinChangeInterval, 60 * time.Second},
		{"summary_interval", cfg.SummaryInterval, 10 * time.Minute},
		{"dry_run", cfg.DryRun, false},
		{"hold_file", cfg.HoldFile, ""},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if !slices.Equal(cfg.Probe.ExpectStatus, []int{200, 204}) {
		t.Errorf("probe.expect_status = %v, want [200 204]", cfg.Probe.ExpectStatus)
	}
}

func TestParseConfigReadsExplicitSettings(t *testing.T) {
	cfg, err := ParseConfig([]byte(minimalConfigYAML + `
ttl: 120
probe:
  path: /healthz
  host: code.dnswatch.test
  interval: 15s
  timeout: 3s
  fail_threshold: 2
  recover_threshold: 4
  expect_status: [204]
min_change_interval: 2m
summary_interval: 30m
hold_file: /etc/clirelay-dnswatch/hold
dry_run: true
alert_webhook: https://hooks.example.test/notify
status_listen: 127.0.0.1:9109
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	p := cfg.Probe
	if p.Path != "/healthz" || p.Host != "code.dnswatch.test" || p.Interval != 15*time.Second ||
		p.Timeout != 3*time.Second || p.FailThreshold != 2 || p.RecoverThreshold != 4 ||
		!slices.Equal(p.ExpectStatus, []int{204}) {
		t.Fatalf("probe settings not applied: %+v", p)
	}
	if cfg.TTL != 120 || cfg.MinChangeInterval != 2*time.Minute || cfg.SummaryInterval != 30*time.Minute ||
		cfg.HoldFile != "/etc/clirelay-dnswatch/hold" || !cfg.DryRun ||
		cfg.AlertWebhook != "https://hooks.example.test/notify" || cfg.StatusListen != "127.0.0.1:9109" {
		t.Fatalf("top-level settings not applied: %+v", cfg)
	}
}

func TestParseConfigRejectsPlaintextToken(t *testing.T) {
	secret := "plain-token-XXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	_, err := ParseConfig([]byte(strings.Replace(minimalConfigYAML,
		"  api_token_file: cf-token\n", "  api_token: "+secret+"\n", 1)))
	if err == nil || !strings.Contains(err.Error(), "api_token_file") {
		t.Fatalf("expected the plaintext token to be refused, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error echoes the token: %v", err)
	}
}

func TestParseConfigRejectsUnknownFields(t *testing.T) {
	_, err := ParseConfig([]byte(minimalConfigYAML + "probe:\n  intervall: 5s\n"))
	if err == nil || !strings.Contains(err.Error(), "intervall") {
		t.Fatalf("expected a typo to be reported, got %v", err)
	}
	if _, err := ParseConfig([]byte("  \n")); err == nil {
		t.Fatal("expected an empty config to be refused")
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{
			Cloudflare: CloudflareConfig{APITokenFile: "/etc/clirelay-dnswatch/cf-token", ZoneID: testZoneID},
			Records:    []string{"relay.dnswatch.test"},
			Nodes:      []Node{{Name: "node-a", IP: "203.0.113.11"}, {Name: "node-b", IP: "203.0.113.12"}},
		}
	}
	if err := base().normalize(); err != nil {
		t.Fatalf("base config must be valid: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing token file", func(c *Config) { c.Cloudflare.APITokenFile = "" }, "api_token_file is required"},
		{"zone id with a slash", func(c *Config) { c.Cloudflare.ZoneID = "abc/../x" }, "zone_id"},
		{"api base url", func(c *Config) { c.Cloudflare.APIBaseURL = "api.cloudflare.com" }, "api_base_url"},
		{"no records", func(c *Config) { c.Records = nil }, "at least one hostname"},
		{"bad hostname", func(c *Config) { c.Records = []string{"not a host"} }, "not a valid hostname"},
		{"wildcard record", func(c *Config) { c.Records = []string{"*.dnswatch.test"} }, "not a valid hostname"},
		{"duplicate record", func(c *Config) { c.Records = []string{"relay.dnswatch.test", "RELAY.dnswatch.test"} }, "listed twice"},
		{"ttl too low", func(c *Config) { c.TTL = 10 }, "ttl must be"},
		{"no nodes", func(c *Config) { c.Nodes = nil }, "at least one node"},
		{"node without name", func(c *Config) { c.Nodes[0].Name = " " }, "name is required"},
		{"duplicate node name", func(c *Config) { c.Nodes[1].Name = "node-a" }, "used twice"},
		{"ipv6 node", func(c *Config) { c.Nodes[0].IP = "2001:db8::1" }, "IPv4"},
		{"unspecified node ip", func(c *Config) { c.Nodes[0].IP = "0.0.0.0" }, "IPv4"},
		{"duplicate node ip", func(c *Config) { c.Nodes[1].IP = "203.0.113.11" }, "used twice"},
		{"relative probe path", func(c *Config) { c.Probe.Path = "readyz" }, "probe.path"},
		{"timeout not below interval", func(c *Config) { c.Probe.Timeout = 10 * time.Second }, "shorter than probe.interval"},
		{"negative threshold", func(c *Config) { c.Probe.FailThreshold = -1 }, "at least 1"},
		{"bogus status", func(c *Config) { c.Probe.ExpectStatus = []int{700} }, "not an HTTP status"},
		{"negative debounce", func(c *Config) { c.MinChangeInterval = -time.Second }, "min_change_interval"},
		{"relative hold file", func(c *Config) { c.HoldFile = "hold" }, "absolute path"},
		{"webhook scheme", func(c *Config) { c.AlertWebhook = "ftp://hooks.example.test/x" }, "alert_webhook"},
		{"status listen without port", func(c *Config) { c.StatusListen = "9109" }, "status_listen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := cfg.normalize()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestConfigNormalizesNodeAddresses(t *testing.T) {
	cfg := &Config{
		Cloudflare: CloudflareConfig{APITokenFile: "/x/cf-token", ZoneID: testZoneID},
		Records:    []string{"relay.dnswatch.test"},
		Nodes:      []Node{{Name: " node-a ", IP: " 203.0.113.11 "}},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Nodes[0] != (Node{Name: "node-a", IP: "203.0.113.11"}) {
		t.Fatalf("node not normalized: %+v", cfg.Nodes[0])
	}
}

func TestLoadConfigResolvesTokenFileNextToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dnswatch.yaml")
	if err := os.WriteFile(path, []byte(minimalConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := filepath.Join(dir, "cf-token"); cfg.Cloudflare.APITokenFile != want {
		t.Fatalf("api_token_file = %q, want %q", cfg.Cloudflare.APITokenFile, want)
	}
}

func TestReadAPIToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	token, err := ReadAPIToken(write("cf-token", "  "+testToken+"\n"))
	if err != nil || token != testToken {
		t.Fatalf("ReadAPIToken = %q, %v; want the trimmed token", token, err)
	}
	if _, err := ReadAPIToken(write("empty", "\n")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected an empty token file to be refused, got %v", err)
	}
	_, err = ReadAPIToken(write("two-lines", testToken+"\nsecond-line\n"))
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("expected a malformed token file to be refused without echoing it, got %v", err)
	}

	// An operator pasting the token where the path belongs must not get it
	// printed back in the startup error.
	pasted := "pastedTokenXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	_, err = ReadAPIToken(filepath.Join(dir, pasted))
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("expected a missing token file error without the pasted value, got %v", err)
	}
	_, err = ReadAPIToken(filepath.Join(dir, "missing-token"))
	if err == nil || !strings.Contains(err.Error(), "missing-token") {
		t.Fatalf("an ordinary missing path should be named in the error, got %v", err)
	}
}

func TestParseConfigReadsEgressPath(t *testing.T) {
	logAttr := func(cfg *Config, key string) any {
		attrs := cfg.LogAttrs()
		for i := 0; i+1 < len(attrs); i += 2 {
			if attrs[i] == key {
				return attrs[i+1]
			}
		}
		return nil
	}

	cfg, err := ParseConfig([]byte(minimalConfigYAML))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Probe.EgressPath != "" || logAttr(cfg, "egress_probe") != "off" {
		t.Fatalf("the egress probe must default to off, got %q", cfg.Probe.EgressPath)
	}

	cfg, err = ParseConfig([]byte(minimalConfigYAML + "probe:\n  egress_path: \" /readyz/egress \"\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Probe.EgressPath != "/readyz/egress" || cfg.Probe.Path != DefaultProbePath {
		t.Fatalf("probe = %+v, want egress_path /readyz/egress next to the default path", cfg.Probe)
	}
	if got := logAttr(cfg, "egress_probe"); got != "https://<node-ip>/readyz/egress" {
		t.Fatalf("egress_probe log attribute = %v", got)
	}
}

func TestConfigValidationOfEgressPath(t *testing.T) {
	cases := []struct {
		name, path, want string
	}{
		{"relative", "readyz/egress", "probe.egress_path must be an absolute path"},
		{"with a host", "//other.dnswatch.test/readyz/egress", "probe.egress_path must be an absolute path"},
		{"same as the readiness path", "/readyz", "probe.egress_path must differ from probe.path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Cloudflare: CloudflareConfig{APITokenFile: "/etc/clirelay-dnswatch/cf-token", ZoneID: testZoneID},
				Records:    []string{"relay.dnswatch.test"},
				Nodes:      []Node{{Name: "node-a", IP: "203.0.113.11"}},
				Probe:      ProbeConfig{EgressPath: tc.path},
			}
			if err := cfg.normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
}
