package config

// ResponseCompressionConfig controls downstream response compression.
type ResponseCompressionConfig struct {
	Enabled  *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MinBytes int   `yaml:"min-bytes,omitempty" json:"min-bytes,omitempty"`
}

func (c ResponseCompressionConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c ResponseCompressionConfig) EffectiveMinBytes() int {
	if c.MinBytes <= 0 {
		return 1024
	}
	return c.MinBytes
}

func (c *Config) EffectiveResponseCompression() ResponseCompressionConfig {
	if c == nil {
		return ResponseCompressionConfig{}
	}
	return c.ServerResponseCompression
}
