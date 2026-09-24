package config

import "strings"

// placeholderAPIKeys are the example client API keys config.example.yaml has shown
// since the first release, active until they were commented out. Every copy of the
// repository publishes them, so presenting one proves nothing about the caller:
// they are never imported and never authenticate, wherever they are configured.
// Matching is exact on purpose; any other value is an ordinary key.
var placeholderAPIKeys = map[string]struct{}{
	"your-api-key-1": {},
	"your-api-key-2": {},
	"your-api-key-3": {},
}

// IsPlaceholderAPIKey reports whether key is one of the example client API keys
// published in config.example.yaml. Surrounding whitespace is ignored, matching
// how keys are normalised before they are stored or compared.
func IsPlaceholderAPIKey(key string) bool {
	_, ok := placeholderAPIKeys[strings.TrimSpace(key)]
	return ok
}
