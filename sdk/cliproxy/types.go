// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// TokenClientProvider loads clients backed by stored authentication tokens.
// It provides an interface for loading authentication tokens from various sources
// and creating clients for AI service providers.
type TokenClientProvider interface {
	// Load loads token-based clients from the configured source.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *TokenClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*TokenClientResult, error)
}

// TokenClientResult represents clients generated from persisted tokens.
// It contains metadata about the loading operation and the number of successful authentications.
type TokenClientResult struct {
	// SuccessfulAuthed is the number of successfully authenticated clients.
	SuccessfulAuthed int
}

// APIKeyClientProvider loads clients backed directly by configured API keys.
// It provides an interface for loading API key-based clients for various AI service providers.
type APIKeyClientProvider interface {
	// Load loads API key-based clients from the configuration.
	//
	// Parameters:
	//   - ctx: The context for the loading operation
	//   - cfg: The application configuration
	//
	// Returns:
	//   - *APIKeyClientResult: The result containing loaded clients
	//   - error: An error if loading fails
	Load(ctx context.Context, cfg *config.Config) (*APIKeyClientResult, error)
}

// APIKeyClientResult is returned by APIKeyClientProvider.Load()
type APIKeyClientResult struct {
	// GeminiKeyCount is the number of Gemini API keys loaded
	GeminiKeyCount int

	// VertexCompatKeyCount is the number of Vertex-compatible API keys loaded
	VertexCompatKeyCount int

	// ClaudeKeyCount is the number of Claude API keys loaded
	ClaudeKeyCount int

	// CodexKeyCount is the number of Codex API keys loaded
	CodexKeyCount int

	// BedrockKeyCount is the number of AWS Bedrock credentials loaded
	BedrockKeyCount int

	// OpenCodeGoKeyCount is the number of OpenCode Go API keys loaded
	OpenCodeGoKeyCount int

	// OllamaCloudKeyCount is the number of Ollama Cloud API keys loaded
	OllamaCloudKeyCount int

	// OpenAICompatCount is the number of OpenAI compatibility API keys loaded
	OpenAICompatCount int
}
