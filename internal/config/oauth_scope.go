package config

// ForAPIKey returns the request-local configuration view for native API-key
// execution. The compatibility configuration has no OAuth-only overlay, so
// the existing immutable view is already correct.
func (cfg *Config) ForAPIKey() *Config { return cfg }
