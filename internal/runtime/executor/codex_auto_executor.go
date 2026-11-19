package executor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets.
//
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) ProbeQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaProbeResult, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.ProbeQuotaRecovery(ctx, auth)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if opts.Alt != "alpha/search" && cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if opts.Alt != "alpha/search" && cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
	// The incremental chains are process-wide and outlive executors, so the
	// "replace every session" signal of a config reload leaves them alone.
	if e.httpExec != nil && sessionID != cliproxyauth.CloseAllExecutionSessionsID {
		e.httpExec.closeIncrementalSession(sessionID)
	}
}
