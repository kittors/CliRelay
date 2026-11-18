package config

import "gopkg.in/yaml.v3"

// ProjectV8ConfigAliases is retained for SDK compatibility. The current
// configuration model already stores the public v8 layout directly.
func ProjectV8ConfigAliases(_ *yaml.Node, _ string) {}

// NormalizeV8ConfigAliases is retained for callers that accept historical
// layouts. Provider and runtime normalization happens during Config decoding.
func NormalizeV8ConfigAliases(_ *yaml.Node) error { return nil }
