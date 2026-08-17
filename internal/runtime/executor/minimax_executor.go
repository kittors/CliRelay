package executor

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// MiniMaxExecutor serves MiniMax credentials.
//
// Chat for this provider is OpenAI-shaped and already handled, so only the image
// endpoint is added here and everything else is delegated unchanged. Registering a
// dedicated executor that reimplemented chat would fork behaviour that has no
// reason to differ.
type MiniMaxExecutor struct {
	*OpenAICompatExecutor
}

// NewMiniMaxExecutor creates an executor bound to a provider key.
func NewMiniMaxExecutor(provider string, cfg *config.Config) *MiniMaxExecutor {
	if strings.TrimSpace(provider) == "" {
		provider = registry.ImageProviderMiniMax
	}
	return &MiniMaxExecutor{OpenAICompatExecutor: NewOpenAICompatExecutor(provider, cfg)}
}

// Execute routes image requests to the image endpoint and everything else to chat.
func (e *MiniMaxExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if minimaxIsMediaAlt(opts.Alt) {
		return e.executeImageGeneration(ctx, auth, req, opts)
	}
	return e.OpenAICompatExecutor.Execute(ctx, auth, req, opts)
}

// ExecuteStream mirrors Execute for the streaming entry point.
func (e *MiniMaxExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if minimaxIsMediaAlt(opts.Alt) {
		return e.executeImageGenerationStream(ctx, auth, req, opts)
	}
	return e.OpenAICompatExecutor.ExecuteStream(ctx, auth, req, opts)
}
