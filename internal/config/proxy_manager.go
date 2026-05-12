package config

import (
	"strings"
	"time"
)

type ProxyManagerConfig struct {
	Assignment ProxyAssignmentConfig `yaml:"proxy-assignment" json:"proxy-assignment"`
	Health     ProxyHealthConfig     `yaml:"proxy-health" json:"proxy-health"`
	BanDetect  ProxyBanDetectConfig  `yaml:"proxy-ban-detect" json:"proxy-ban-detect"`
}

type ProxyAssignmentStrategy string

const (
	ProxyAssignmentSpread     ProxyAssignmentStrategy = "spread"
	ProxyAssignmentRoundRobin ProxyAssignmentStrategy = "round-robin"
	ProxyAssignmentRandom     ProxyAssignmentStrategy = "random"
)

type ProxyAssignmentConfig struct {
	Enabled   bool                   `yaml:"enabled" json:"enabled"`
	Strategy  ProxyAssignmentStrategy `yaml:"strategy" json:"strategy"`
	Providers []string               `yaml:"providers,omitempty" json:"providers,omitempty"`
}

type ProxyHealthConfig struct {
	Enabled          bool          `yaml:"enabled" json:"enabled"`
	Interval         time.Duration `yaml:"interval" json:"interval"`
	Timeout          time.Duration `yaml:"timeout" json:"timeout"`
	FailureThreshold int           `yaml:"failure-threshold" json:"failure-threshold"`
	RecoveryInterval time.Duration `yaml:"recovery-interval" json:"recovery-interval"`
	TestURL          string        `yaml:"test-url" json:"test-url"`
}

type ProxyBanDetectConfig struct {
	Enabled          bool `yaml:"enabled" json:"enabled"`
	BanThreshold     int  `yaml:"ban-threshold" json:"ban-threshold"`
	WindowMinutes    int  `yaml:"window-minutes" json:"window-minutes"`
	AutoDisableProxy bool `yaml:"auto-disable-proxy" json:"auto-disable-proxy"`
}

func (c *Config) SanitizeProxyManager() {
	pm := &c.ProxyManager

	pm.Assignment.Strategy = ProxyAssignmentStrategy(strings.TrimSpace(string(pm.Assignment.Strategy)))
	if pm.Assignment.Strategy == "" {
		pm.Assignment.Strategy = ProxyAssignmentSpread
	}

	if pm.Health.Enabled {
		if pm.Health.Interval <= 0 {
			pm.Health.Interval = 5 * time.Minute
		}
		if pm.Health.Timeout <= 0 {
			pm.Health.Timeout = 10 * time.Second
		}
		if pm.Health.FailureThreshold <= 0 {
			pm.Health.FailureThreshold = 3
		}
		if pm.Health.RecoveryInterval <= 0 {
			pm.Health.RecoveryInterval = 15 * time.Minute
		}
		if pm.Health.TestURL == "" {
			pm.Health.TestURL = "https://api.openai.com/v1/models"
		}
	}

	if pm.BanDetect.Enabled {
		if pm.BanDetect.BanThreshold <= 0 {
			pm.BanDetect.BanThreshold = 3
		}
		if pm.BanDetect.WindowMinutes <= 0 {
			pm.BanDetect.WindowMinutes = 60
		}
	}
}

func (c *Config) IsProxyManagerEnabled() bool {
	pm := &c.ProxyManager
	return pm.Assignment.Enabled || pm.Health.Enabled || pm.BanDetect.Enabled
}
