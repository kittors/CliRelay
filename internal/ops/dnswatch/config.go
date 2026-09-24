// Package dnswatch keeps the Cloudflare A records of the CliRelay front-door
// hostnames in line with which application nodes are actually serving.
//
// It runs on a third (arbiter) machine, probes every node's readiness endpoint
// over the same HTTPS path clients use, and adds or removes each node's A
// record as nodes fail and recover. It never deletes the last healthy record
// and never touches records that do not belong to a configured node.
package dnswatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied to settings that are omitted (left at zero).
const (
	DefaultAPIBaseURL        = "https://api.cloudflare.com/client/v4"
	DefaultTTL               = 60
	DefaultProbePath         = "/readyz"
	DefaultProbeInterval     = 10 * time.Second
	DefaultProbeTimeout      = 5 * time.Second
	DefaultFailThreshold     = 3
	DefaultRecoverThreshold  = 3
	DefaultMinChangeInterval = 60 * time.Second
	DefaultSummaryInterval   = 10 * time.Minute
)

// defaultExpectStatus matches /readyz, which answers 204 when ready; 200 is
// accepted for readiness endpoints behind proxies that rewrite empty replies.
var defaultExpectStatus = []int{200, 204}

var (
	zoneIDPattern   = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tokenLikeName   = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`)
)

// Config is the YAML configuration of clirelay-dnswatch.
type Config struct {
	Cloudflare CloudflareConfig `yaml:"cloudflare"`
	// Records are the hostnames whose A record set follows node health.
	Records []string `yaml:"records"`
	// TTL applies to every record dnswatch creates. Records are always
	// created unproxied (grey cloud) so clients connect to the nodes directly.
	TTL   int         `yaml:"ttl"`
	Nodes []Node      `yaml:"nodes"`
	Probe ProbeConfig `yaml:"probe"`
	// MinChangeInterval is the minimum time between two state flips of the
	// same node, so a flapping node cannot churn DNS every few probes.
	MinChangeInterval time.Duration `yaml:"min_change_interval"`
	// SummaryInterval spaces the periodic status line in the log.
	SummaryInterval time.Duration `yaml:"summary_interval"`
	// HoldFile freezes DNS while it exists; probing and state tracking go on.
	HoldFile string `yaml:"hold_file"`
	// DryRun computes and logs every DNS change without calling write APIs.
	DryRun bool `yaml:"dry_run"`
	// AlertWebhook receives a JSON POST for state changes and DNS updates.
	AlertWebhook string `yaml:"alert_webhook"`
	// StatusListen serves GET /status when set, e.g. 127.0.0.1:9109.
	StatusListen string `yaml:"status_listen"`
}

// CloudflareConfig locates the zone and the API token.
type CloudflareConfig struct {
	// APITokenFile holds the API token. A relative path resolves against the
	// config file's directory, so one config works both from /etc and from a
	// systemd credentials directory.
	APITokenFile string `yaml:"api_token_file"`
	ZoneID       string `yaml:"zone_id"`
	// APIBaseURL is overridable for tests; production uses the default.
	APIBaseURL string `yaml:"api_base_url"`
	// PlaintextToken exists only so a token pasted into the config is refused
	// with a clear message rather than a generic unknown-field error.
	PlaintextToken string `yaml:"api_token"`
}

// Node is one application server that may serve the records.
type Node struct {
	Name string `yaml:"name" json:"name"`
	IP   string `yaml:"ip" json:"ip"`
}

// ProbeConfig controls the readiness probe sent to every node.
type ProbeConfig struct {
	Path string `yaml:"path"`
	// Host is both the TLS server name and the HTTP Host header; it defaults
	// to the first record so the node's public certificate is verified.
	Host             string        `yaml:"host"`
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailThreshold    int           `yaml:"fail_threshold"`
	RecoverThreshold int           `yaml:"recover_threshold"`
	ExpectStatus     []int         `yaml:"expect_status"`
}

var errPlaintextToken = errors.New("cloudflare.api_token is not accepted: write the token to a file readable only by the service and set cloudflare.api_token_file")

// LoadConfig reads, defaults and validates the config file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if !filepath.IsAbs(cfg.Cloudflare.APITokenFile) {
		absConfig, errAbs := filepath.Abs(path)
		if errAbs != nil {
			return nil, fmt.Errorf("resolve config path: %w", errAbs)
		}
		cfg.Cloudflare.APITokenFile = filepath.Join(filepath.Dir(absConfig), cfg.Cloudflare.APITokenFile)
	}
	return cfg, nil
}

// ParseConfig decodes YAML strictly (unknown keys are errors), fills in
// defaults and validates the result.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalize fills defaults and validates. It is idempotent so New can apply
// it to configs built in code as well as parsed ones.
func (c *Config) normalize() error {
	if c.Cloudflare.PlaintextToken != "" {
		c.Cloudflare.PlaintextToken = ""
		return errPlaintextToken
	}
	c.applyDefaults()
	return c.validate()
}

func (c *Config) applyDefaults() {
	c.Cloudflare.APITokenFile = strings.TrimSpace(c.Cloudflare.APITokenFile)
	c.Cloudflare.ZoneID = strings.TrimSpace(c.Cloudflare.ZoneID)
	c.Cloudflare.APIBaseURL = strings.TrimRight(strings.TrimSpace(c.Cloudflare.APIBaseURL), "/")
	if c.Cloudflare.APIBaseURL == "" {
		c.Cloudflare.APIBaseURL = DefaultAPIBaseURL
	}
	for i := range c.Records {
		c.Records[i] = normalizeHostname(c.Records[i])
	}
	if c.TTL == 0 {
		c.TTL = DefaultTTL
	}
	for i := range c.Nodes {
		c.Nodes[i].Name = strings.TrimSpace(c.Nodes[i].Name)
		c.Nodes[i].IP = strings.TrimSpace(c.Nodes[i].IP)
		if addr, err := netip.ParseAddr(c.Nodes[i].IP); err == nil {
			c.Nodes[i].IP = addr.String()
		}
	}

	p := &c.Probe
	p.Path = strings.TrimSpace(p.Path)
	if p.Path == "" {
		p.Path = DefaultProbePath
	}
	p.Host = normalizeHostname(p.Host)
	if p.Host == "" && len(c.Records) > 0 {
		p.Host = c.Records[0]
	}
	if p.Interval == 0 {
		p.Interval = DefaultProbeInterval
	}
	if p.Timeout == 0 {
		p.Timeout = DefaultProbeTimeout
	}
	if p.FailThreshold == 0 {
		p.FailThreshold = DefaultFailThreshold
	}
	if p.RecoverThreshold == 0 {
		p.RecoverThreshold = DefaultRecoverThreshold
	}
	if len(p.ExpectStatus) == 0 {
		p.ExpectStatus = append([]int(nil), defaultExpectStatus...)
	}

	if c.MinChangeInterval == 0 {
		c.MinChangeInterval = DefaultMinChangeInterval
	}
	if c.SummaryInterval == 0 {
		c.SummaryInterval = DefaultSummaryInterval
	}
	c.HoldFile = strings.TrimSpace(c.HoldFile)
	c.AlertWebhook = strings.TrimSpace(c.AlertWebhook)
	c.StatusListen = strings.TrimSpace(c.StatusListen)
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Cloudflare.APITokenFile == "" {
		fail("cloudflare.api_token_file is required")
	}
	if !zoneIDPattern.MatchString(c.Cloudflare.ZoneID) {
		fail("cloudflare.zone_id must be the zone's alphanumeric id")
	}
	if !isHTTPURL(c.Cloudflare.APIBaseURL) {
		fail("cloudflare.api_base_url must be an http(s) URL")
	}

	if len(c.Records) == 0 {
		fail("records must list at least one hostname")
	}
	seenRecords := make(map[string]bool, len(c.Records))
	for _, name := range c.Records {
		if !hostnamePattern.MatchString(name) {
			fail("records: %q is not a valid hostname", name)
		}
		if seenRecords[name] {
			fail("records: %q is listed twice", name)
		}
		seenRecords[name] = true
	}
	// Cloudflare takes 1 ("automatic") or an explicit 30..86400 seconds.
	if c.TTL != 1 && (c.TTL < 30 || c.TTL > 86400) {
		fail("ttl must be 1 (automatic) or between 30 and 86400 seconds")
	}

	if len(c.Nodes) == 0 {
		fail("nodes must list at least one node")
	}
	seenNames := make(map[string]bool, len(c.Nodes))
	seenIPs := make(map[string]bool, len(c.Nodes))
	for i, node := range c.Nodes {
		switch {
		case node.Name == "":
			fail("nodes[%d].name is required", i)
		case seenNames[node.Name]:
			fail("nodes[%d].name %q is used twice", i, node.Name)
		}
		seenNames[node.Name] = true
		addr, err := netip.ParseAddr(node.IP)
		switch {
		case err != nil || !addr.Is4() || addr.IsUnspecified():
			fail("nodes[%d].ip %q must be an IPv4 address (dnswatch manages A records)", i, node.IP)
		case seenIPs[node.IP]:
			fail("nodes[%d].ip %s is used twice", i, node.IP)
		}
		seenIPs[node.IP] = true
	}

	p := c.Probe
	if parsed, err := url.Parse(p.Path); err != nil || !strings.HasPrefix(p.Path, "/") || parsed.Host != "" {
		fail("probe.path must be an absolute path such as /readyz")
	}
	if !hostnamePattern.MatchString(p.Host) {
		fail("probe.host %q is not a valid hostname", p.Host)
	}
	if p.Interval < 0 || p.Timeout < 0 {
		fail("probe.interval and probe.timeout must be positive")
	} else if p.Timeout >= p.Interval {
		fail("probe.timeout (%s) must be shorter than probe.interval (%s)", p.Timeout, p.Interval)
	}
	if p.FailThreshold < 1 || p.RecoverThreshold < 1 {
		fail("probe.fail_threshold and probe.recover_threshold must be at least 1")
	}
	for _, code := range p.ExpectStatus {
		if code < 100 || code > 599 {
			fail("probe.expect_status: %d is not an HTTP status code", code)
		}
	}

	if c.MinChangeInterval < 0 {
		fail("min_change_interval must not be negative")
	}
	if c.SummaryInterval < 0 {
		fail("summary_interval must not be negative")
	}
	if c.HoldFile != "" && !filepath.IsAbs(c.HoldFile) {
		fail("hold_file must be an absolute path")
	}
	// The webhook URL is left out of the message: chat webhooks embed secrets.
	if c.AlertWebhook != "" && !isHTTPURL(c.AlertWebhook) {
		fail("alert_webhook must be an http(s) URL")
	}
	if c.StatusListen != "" {
		if _, port, err := net.SplitHostPort(c.StatusListen); err != nil || !isPort(port) {
			fail("status_listen %q must be host:port, e.g. 127.0.0.1:9109", c.StatusListen)
		}
	}
	return errors.Join(errs...)
}

// ReadAPIToken reads the Cloudflare API token from path. Errors never echo
// the token, and hide the path when it looks like a token pasted in place of
// a file name.
func ReadAPIToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return "", fmt.Errorf("read cloudflare api token file %s: %w", DescribeTokenPath(path), err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("cloudflare api token file %s is empty", DescribeTokenPath(path))
	}
	for _, r := range token {
		if r <= ' ' || r > '~' {
			return "", fmt.Errorf("cloudflare api token file %s must contain only the token", DescribeTokenPath(path))
		}
	}
	return token, nil
}

// DescribeTokenPath renders the token file path for messages.
func DescribeTokenPath(path string) string {
	if tokenLikeName.MatchString(filepath.Base(path)) {
		return "(path hidden: it looks like a token, not a file name)"
	}
	return strconv.Quote(path)
}

// LogAttrs describes the effective config for logs. It carries no secret:
// the token is never part of Config, and only the webhook's origin is shown.
func (c *Config) LogAttrs() []any {
	nodes := make([]string, 0, len(c.Nodes))
	for _, node := range c.Nodes {
		nodes = append(nodes, node.Name+"="+node.IP)
	}
	return []any{
		"zone_id", c.Cloudflare.ZoneID,
		"api_token_file", DescribeTokenPath(c.Cloudflare.APITokenFile),
		"records", strings.Join(c.Records, ","),
		"ttl", c.TTL,
		"nodes", strings.Join(nodes, ","),
		"probe", "https://<node-ip>" + c.Probe.Path,
		"probe_host", c.Probe.Host,
		"interval", c.Probe.Interval.String(),
		"timeout", c.Probe.Timeout.String(),
		"fail_threshold", c.Probe.FailThreshold,
		"recover_threshold", c.Probe.RecoverThreshold,
		"expect_status", fmt.Sprint(c.Probe.ExpectStatus),
		"min_change_interval", c.MinChangeInterval.String(),
		"dry_run", c.DryRun,
		"hold_file", c.HoldFile,
		"alert_webhook", webhookOrigin(c.AlertWebhook),
		"status_listen", c.StatusListen,
	}
}

func normalizeHostname(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func isPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}
