package executor

import (
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// egressBreaker is the breaker every proxied upstream connection of this
// package passes: the HTTP clients of newProxyAwareHTTPClient and the Codex
// websocket dial. It is the process-wide one, which the scheduler consults
// before it calls an executor. Tests replace it.
var egressBreaker = egressbreaker.Default()

// Each executor names the proxy a credential's traffic leaves through
// (cliproxyauth.EgressProxyResolver), so that the scheduler can skip an
// account whose proxy the breaker has found broken before the executor
// translates the request for it.
//
// The answer has to be the URL newProxyAwareHTTPClient resolves for the same
// executor and credential, because the breaker keys its state by it; both go
// through resolveAuthProxyURL with the configuration the executor was bound
// with. An executor that cannot give one answer for all of its requests
// returns "" and is simply not skipped ahead of time: its transports still
// pass the breaker. TestExecutorsResolveTheirEgressProxy checks every
// executor.
//
// AIStudioExecutor has no method: its traffic goes through the websocket
// relay, not a proxy.

func (e *AntigravityExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *BedrockExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *ClaudeExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

// CodexWebsocketsExecutor gets this one through the CodexExecutor it embeds.
func (e *CodexExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *CodexAutoExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return e.httpExec.EgressProxyURL(auth)
}

func (e *GeminiCLIExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *GeminiExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *GeminiVertexExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *IFlowExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

// KimiExecutor serves Claude-format requests through a ClaudeExecutor built
// from the same configuration, so both of its formats leave through this proxy.
func (e *KimiExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *OllamaCloudExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

// MiniMaxExecutor gets this one through the OpenAICompatExecutor it embeds.
func (e *OpenAICompatExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *OpenCodeGoExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *QwenExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}

func (e *XAIExecutor) EgressProxyURL(auth *cliproxyauth.Auth) string {
	if e == nil {
		return ""
	}
	return resolveAuthProxyURL(e.cfg, auth)
}
