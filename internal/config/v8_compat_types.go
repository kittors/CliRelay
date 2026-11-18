package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

// PluginsConfig holds dynamic plugin system settings.
type PluginsConfig struct {
	Enabled      bool                            `yaml:"enabled" json:"enabled"`
	Dir          string                          `yaml:"dir" json:"dir"`
	StoreSources []string                        `yaml:"store-sources,omitempty" json:"store-sources,omitempty"`
	StoreAuth    []sdkpluginstore.AuthConfig     `yaml:"store-auth,omitempty" json:"store-auth,omitempty"`
	AuthRevision int64                           `yaml:"auth-revision,omitempty" json:"auth-revision,omitempty"`
	Configs      map[string]PluginInstanceConfig `yaml:"configs" json:"configs"`
}

// PluginInstanceConfig stores host-owned plugin settings and raw plugin YAML.
type PluginInstanceConfig struct {
	Enabled  *bool
	Priority int
	Raw      yaml.Node
}

func (c *PluginInstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil {
		return nil
	}
	enabled := false
	c.Enabled = &enabled
	c.Priority = 0
	if value == nil || value.Kind == 0 {
		return nil
	}
	c.Raw = *value
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key, node := value.Content[i], value.Content[i+1]
		switch key.Value {
		case "enabled":
			if err := node.Decode(&enabled); err != nil {
				return fmt.Errorf("parse plugin enabled: %w", err)
			}
			c.Enabled = &enabled
		case "priority":
			if err := node.Decode(&c.Priority); err != nil {
				return fmt.Errorf("parse plugin priority: %w", err)
			}
		}
	}
	return nil
}

func (c PluginInstanceConfig) MarshalYAML() (any, error) {
	if c.Raw.Kind != 0 {
		return &c.Raw, nil
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
}

// CodexConfig configures provider-wide Codex request behavior.
type CodexConfig struct {
	WebsocketCompression               *bool                     `yaml:"websocket-compression,omitempty" json:"websocket-compression,omitempty"`
	UpstreamRequestCompression         bool                      `yaml:"upstream-request-compression" json:"upstream-request-compression"`
	UpstreamRequestCompressionMinBytes int                       `yaml:"upstream-request-compression-min-bytes,omitempty" json:"upstream-request-compression-min-bytes,omitempty"`
	DisableCodexCloaking               bool                      `yaml:"disable-codex-cloaking" json:"disable-codex-cloaking"`
	StreamBootstrapBuffering           bool                      `yaml:"stream-bootstrap-buffering" json:"stream-bootstrap-buffering"`
	StreamBootstrapTimeout             string                    `yaml:"stream-bootstrap-timeout,omitempty" json:"stream-bootstrap-timeout,omitempty"`
	OrphanDelegationCompatibility      bool                      `yaml:"orphan-delegation-compatibility" json:"orphan-delegation-compatibility"`
	ModelLevelCooling                  bool                      `yaml:"model-level-cooling" json:"model-level-cooling"`
	LiveMediaRelay                     CodexLiveMediaRelayConfig `yaml:"live-media-relay" json:"live-media-relay"`
	ResponseSteering                   bool                      `yaml:"response-steering" json:"response-steering"`
	SignInWithChatGPTIdentifier        string                    `yaml:"signin-with-chatgpt-identifier,omitempty" json:"signin-with-chatgpt-identifier,omitempty"`
}

const (
	DefaultSignInWithChatGPTIdentifier = "CLIProxyAPI"
	DefaultCodexStreamBootstrapTimeout = 0
)

const maxBootstrapTimeoutSeconds = int64(math.MaxInt64 / int64(time.Second))

// CodexLiveMediaRelayConfig configures the in-process Codex Live WebRTC gateway.
type CodexLiveMediaRelayConfig struct {
	Enabled                 bool                 `yaml:"enabled" json:"enabled"`
	MaxSessions             int                  `yaml:"max-sessions" json:"max-sessions"`
	DisablePrivateRemoteIPs bool                 `yaml:"disable-private-remote-ips" json:"disable-private-remote-ips"`
	PublicIP                string               `yaml:"public-ip" json:"public-ip"`
	UDPPortMin              uint16               `yaml:"udp-port-min" json:"udp-port-min"`
	UDPPortMax              uint16               `yaml:"udp-port-max" json:"udp-port-max"`
	ICEServers              []CodexLiveICEServer `yaml:"ice-servers" json:"ice-servers"`
}

func (c *CodexConfig) WebsocketCompressionEnabled() bool {
	return c == nil || c.WebsocketCompression == nil || *c.WebsocketCompression
}

func (c *CodexConfig) RequestCompressionMinBytes() int {
	if c == nil || c.UpstreamRequestCompressionMinBytes <= 0 {
		return 1024
	}
	return c.UpstreamRequestCompressionMinBytes
}

func (c CodexConfig) EffectiveSignInWithChatGPTIdentifier() string {
	if v := strings.TrimSpace(c.SignInWithChatGPTIdentifier); v != "" {
		return v
	}
	return DefaultSignInWithChatGPTIdentifier
}

func (c *CodexConfig) StreamBootstrapTimeoutDuration() time.Duration {
	if c == nil {
		return DefaultCodexStreamBootstrapTimeout
	}
	raw := strings.TrimSpace(c.StreamBootstrapTimeout)
	if raw == "" || raw == "0" || strings.EqualFold(raw, "none") ||
		strings.EqualFold(raw, "unlimited") || strings.EqualFold(raw, "disabled") ||
		strings.EqualFold(raw, "off") || strings.EqualFold(raw, "never") {
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs >= 0 && secs <= maxBootstrapTimeoutSeconds {
		return time.Duration(secs) * time.Second
	}
	return DefaultCodexStreamBootstrapTimeout
}

// HomeConfig stores runtime-only Home control-plane settings.
type HomeConfig struct {
	Enabled                 bool          `yaml:"enabled" json:"enabled"`
	NodeID                  string        `yaml:"-" json:"-"`
	Host                    string        `yaml:"host" json:"-"`
	Port                    int           `yaml:"port" json:"-"`
	DisableClusterDiscovery bool          `yaml:"disable-cluster-discovery" json:"-"`
	TLS                     HomeTLSConfig `yaml:"tls" json:"-"`
}

type HomeTLSConfig struct {
	Enable              bool   `yaml:"enable" json:"-"`
	ServerName          string `yaml:"server-name" json:"-"`
	InsecureSkipVerify  bool   `yaml:"insecure-skip-verify" json:"-"`
	CACert              string `yaml:"ca-cert" json:"-"`
	ClientCert          string `yaml:"-" json:"-"`
	ClientKey           string `yaml:"-" json:"-"`
	UseTargetServerName bool   `yaml:"-" json:"-"`
}

// DiscoveryConfig controls local network service advertising.
type DiscoveryConfig struct {
	Enabled             bool                      `yaml:"enabled" json:"enabled"`
	ServiceName         string                    `yaml:"service-name" json:"service-name"`
	ServiceType         string                    `yaml:"service-type" json:"service-type"`
	Subtypes            []string                  `yaml:"subtypes" json:"subtypes"`
	Interfaces          DiscoveryInterfacesConfig `yaml:"interfaces" json:"interfaces"`
	AuthRequired        *bool                     `yaml:"auth-required" json:"auth-required"`
	AdvertiseManagement bool                      `yaml:"advertise-management" json:"advertise-management"`
}

type DiscoveryInterfacesConfig struct {
	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude" json:"exclude"`
}

const defaultPluginsDir = "plugins"

func ResolvePluginsDir(pluginsDir string) (string, error) {
	pluginsDir = strings.TrimSpace(pluginsDir)
	if pluginsDir == "" {
		pluginsDir = defaultPluginsDir
	}
	if strings.HasPrefix(pluginsDir, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve plugins directory: %w", err)
		}
		remainder := strings.TrimLeft(strings.TrimPrefix(pluginsDir, "~"), "/\\")
		if remainder == "" {
			return filepath.Clean(homeDir), nil
		}
		return filepath.Clean(filepath.Join(homeDir, filepath.FromSlash(strings.ReplaceAll(remainder, "\\", "/")))), nil
	}
	return filepath.Clean(pluginsDir), nil
}

func (cfg *Config) ResolvePluginsDir() error {
	if cfg == nil {
		return nil
	}
	dir, err := ResolvePluginsDir(cfg.Plugins.Dir)
	if err != nil {
		return err
	}
	cfg.Plugins.Dir = dir
	return nil
}

// NormalizePluginsConfig applies default plugin configuration values.
func (cfg *Config) NormalizePluginsConfig() {
	if cfg == nil {
		return
	}
	cfg.Plugins.Dir = strings.TrimSpace(cfg.Plugins.Dir)
	if cfg.Plugins.Dir == "" {
		cfg.Plugins.Dir = defaultPluginsDir
	}
	if cfg.Plugins.Configs == nil {
		cfg.Plugins.Configs = map[string]PluginInstanceConfig{}
	}
}
