package auth

import (
	"context"
	"net/http"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executionService struct {
	manager *Manager
}

type mixedExecutionScope struct {
	providers       []string
	routeModel      string
	opts            cliproxyexecutor.Options
	singlePickRoute bool
	tried           map[string]struct{}
}

type mixedExecutionCandidate struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
	execCtx  context.Context
	execReq  cliproxyexecutor.Request
}

func newExecutionService(manager *Manager) executionService {
	return executionService{manager: manager}
}

func (s executionService) execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return runExecutionWithRetry(s.manager, ctx, providers, req, opts, s.executeMixedOnce)
}

func (s executionService) executeCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return runExecutionWithRetry(s.manager, ctx, providers, req, opts, s.executeCountMixedOnce)
}

func (s executionService) executeStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return runExecutionWithRetry(s.manager, ctx, providers, req, opts, s.executeStreamMixedOnce)
}

func runExecutionWithRetry[T any](
	manager *Manager,
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	executeOnce func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options) (T, error),
) (T, error) {
	var zero T

	normalized := manager.normalizeProviders(providers)
	if len(normalized) == 0 {
		return zero, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxWait := manager.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := executeOnce(ctx, normalized, req, opts)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := manager.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait, opts.Metadata)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return zero, errWait
		}
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (s executionService) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	scope, err := s.newMixedScope(providers, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	var lastErr error
	for {
		candidate, errPick := s.nextMixedCandidate(ctx, &scope, req)
		if errPick != nil {
			return cliproxyexecutor.Response{}, resolveMixedPickError(lastErr, errPick)
		}

		resp, errExec := candidate.executor.Execute(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		result := Result{
			AuthID:   candidate.auth.ID,
			Provider: candidate.provider,
			Model:    scope.routeModel,
			Success:  errExec == nil,
		}
		if errExec != nil {
			if errCtx := candidate.execCtx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
			result.Error = errorFromExecution(errExec)
			result.Headers = headersFromError(errExec)
			if ra := retryAfterFromError(errExec); ra != nil {
				result.RetryAfter = ra
			}
			s.manager.MarkResult(candidate.execCtx, result)
			if isRequestInvalidError(errExec) || scope.singlePickRoute {
				return cliproxyexecutor.Response{}, errExec
			}
			lastErr = errExec
			continue
		}

		result.Headers = resp.Headers.Clone()
		s.manager.MarkResult(candidate.execCtx, result)
		return resp, nil
	}
}

func (s executionService) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	scope, err := s.newMixedScope(providers, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	var lastErr error
	for {
		candidate, errPick := s.nextMixedCandidate(ctx, &scope, req)
		if errPick != nil {
			return cliproxyexecutor.Response{}, resolveMixedPickError(lastErr, errPick)
		}

		resp, errExec := candidate.executor.CountTokens(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		if errExec != nil {
			if errCtx := candidate.execCtx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
			if isRequestInvalidError(errExec) || scope.singlePickRoute {
				return cliproxyexecutor.Response{}, errExec
			}
			lastErr = errExec
			continue
		}

		return resp, nil
	}
}

func (s executionService) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	scope, err := s.newMixedScope(providers, req, opts)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for {
		candidate, errPick := s.nextMixedCandidate(ctx, &scope, req)
		if errPick != nil {
			return nil, resolveMixedPickError(lastErr, errPick)
		}

		streamResult, errStream := candidate.executor.ExecuteStream(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		if errStream != nil {
			if errCtx := candidate.execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			rerr := errorFromExecution(errStream)
			result := Result{
				AuthID:     candidate.auth.ID,
				Provider:   candidate.provider,
				Model:      scope.routeModel,
				Success:    false,
				Error:      rerr,
				RetryAfter: retryAfterFromError(errStream),
				Headers:    headersFromError(errStream),
			}
			s.manager.MarkResult(candidate.execCtx, result)
			if isRequestInvalidError(errStream) || scope.singlePickRoute {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		return s.wrapStreamResult(candidate.execCtx, candidate.auth, candidate.provider, scope.routeModel, streamResult), nil
	}
}

func (s executionService) newMixedScope(providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (mixedExecutionScope, error) {
	if len(providers) == 0 {
		return mixedExecutionScope{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)

	return mixedExecutionScope{
		providers:       providers,
		routeModel:      routeModel,
		opts:            opts,
		singlePickRoute: isSinglePickRouteRequest(opts.Metadata),
		tried:           make(map[string]struct{}),
	}, nil
}

func (s executionService) nextMixedCandidate(ctx context.Context, scope *mixedExecutionScope, req cliproxyexecutor.Request) (*mixedExecutionCandidate, error) {
	auth, executor, provider, errPick := s.manager.pickNextMixed(ctx, scope.providers, scope.routeModel, scope.opts, scope.tried)
	if errPick != nil {
		return nil, errPick
	}

	entry := logEntryWithRequestID(ctx)
	debugLogAuthSelection(entry, auth, provider, req.Model)
	publishSelectedAuthMetadata(scope.opts.Metadata, auth.ID)
	scope.tried[auth.ID] = struct{}{}

	return &mixedExecutionCandidate{
		auth:     auth,
		executor: executor,
		provider: provider,
		execCtx:  s.executionContext(ctx, auth),
		execReq:  s.rewriteRequestForAuth(req, scope.routeModel, auth),
	}, nil
}

func (s executionService) executionContext(ctx context.Context, auth *Auth) context.Context {
	if rt := s.manager.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = cliproxyexecutor.WithRoundTripper(ctx, rt)
	}
	return ctx
}

func (s executionService) rewriteRequestForAuth(req cliproxyexecutor.Request, routeModel string, auth *Auth) cliproxyexecutor.Request {
	execReq := req
	execReq.Model = rewriteModelForAuth(routeModel, auth)
	execReq.Model = s.manager.applyOAuthModelAlias(auth, execReq.Model)
	execReq.Model = s.manager.applyAPIKeyModelAlias(auth, execReq.Model)
	return execReq
}

func (s executionService) wrapStreamResult(
	execCtx context.Context,
	auth *Auth,
	provider string,
	routeModel string,
	streamResult *cliproxyexecutor.StreamResult,
) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	streamHeaders := streamResult.Headers.Clone()
	go func(streamCtx context.Context, streamAuth *Auth, streamProvider string, streamChunks <-chan cliproxyexecutor.StreamChunk, headers http.Header) {
		defer close(out)
		var failed bool
		forward := true
		for chunk := range streamChunks {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := errorFromExecution(chunk.Err)
				chunkHeaders := headersFromError(chunk.Err)
				if len(chunkHeaders) == 0 {
					chunkHeaders = headers.Clone()
				}
				s.manager.MarkResult(streamCtx, Result{
					AuthID:     streamAuth.ID,
					Provider:   streamProvider,
					Model:      routeModel,
					Success:    false,
					Error:      rerr,
					RetryAfter: retryAfterFromError(chunk.Err),
					Headers:    chunkHeaders,
				})
			}
			if !forward {
				continue
			}
			if streamCtx == nil {
				out <- chunk
				continue
			}
			select {
			case <-streamCtx.Done():
				forward = false
			case out <- chunk:
			}
		}
		if !failed {
			s.manager.MarkResult(streamCtx, Result{
				AuthID:   streamAuth.ID,
				Provider: streamProvider,
				Model:    routeModel,
				Success:  true,
				Headers:  headers.Clone(),
			})
		}
	}(execCtx, auth.Clone(), provider, streamResult.Chunks, streamHeaders)
	return &cliproxyexecutor.StreamResult{
		Headers: streamHeaders.Clone(),
		Chunks:  out,
	}
}

func resolveMixedPickError(lastErr error, errPick error) error {
	if lastErr != nil {
		return lastErr
	}
	return errPick
}
