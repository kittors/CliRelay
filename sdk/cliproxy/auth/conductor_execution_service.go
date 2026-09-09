package auth

import (
	"context"
	"errors"
	"net/http"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// errNoSaturatedCandidate marks "there was nothing to queue on", which is an
// internal control signal rather than the reason a request failed.
var errNoSaturatedCandidate = errors.New("no saturated candidate to wait for")

type executionService struct {
	manager *Manager
}

type mixedExecutionScope struct {
	providers       []string
	routeModel      string
	opts            cliproxyexecutor.Options
	singlePickRoute bool
	tried           map[string]struct{}
	// saturated collects candidates skipped because their account was already at
	// its concurrency limit. Once no idle candidate is left they become the queue
	// this request waits on.
	saturated []*mixedExecutionCandidate
	// lastErr keeps the most recent failover reason so an exhausted candidate list
	// reports why every candidate was rejected instead of a generic "no auth".
	lastErr error
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
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}

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

	for {
		candidate, releaseSlot, errReady := s.nextReadyCandidate(ctx, &scope, req)
		if errReady != nil {
			return cliproxyexecutor.Response{}, errReady
		}

		resp, errExec := candidate.executor.Execute(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		releaseSlot()
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
			scope.lastErr = errExec
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

	for {
		candidate, releaseSlot, errReady := s.nextReadyCandidate(ctx, &scope, req)
		if errReady != nil {
			return cliproxyexecutor.Response{}, errReady
		}

		resp, errExec := candidate.executor.CountTokens(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		releaseSlot()
		if errExec != nil {
			if errCtx := candidate.execCtx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
			if isRequestInvalidError(errExec) || scope.singlePickRoute {
				return cliproxyexecutor.Response{}, errExec
			}
			scope.lastErr = errExec
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

	for {
		candidate, releaseSlot, errReady := s.nextReadyCandidate(ctx, &scope, req)
		if errReady != nil {
			return nil, errReady
		}

		streamResult, errStream := candidate.executor.ExecuteStream(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
		if errStream != nil {
			releaseSlot()
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
			scope.lastErr = errStream
			continue
		}

		return s.wrapStreamResult(candidate.execCtx, candidate.auth, candidate.provider, scope.routeModel, scope.opts.SourceFormat, streamResult, releaseSlot), nil
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

// nextReadyCandidate returns the next candidate that already owns a concurrency
// slot, along with the release function the caller must invoke.
//
// Idle accounts are taken immediately so a healthy pool keeps its low latency.
// Only when every candidate is saturated does the request queue on them, which is
// what makes a per-account limit a throttle instead of an error: a single-pick
// route (pinned auth or sticky session) has nowhere to fail over to, so failing
// fast there would reject requests the account could serve moments later.
func (s executionService) nextReadyCandidate(ctx context.Context, scope *mixedExecutionScope, req cliproxyexecutor.Request) (*mixedExecutionCandidate, func(), error) {
	for {
		candidate, errPick := s.nextMixedCandidate(ctx, scope, req)
		if errPick != nil {
			queued, releaseSlot, errWait := s.waitForSaturatedSlot(ctx, scope)
			if errWait == nil {
				return queued, releaseSlot, nil
			}
			if !errors.Is(errWait, errNoSaturatedCandidate) {
				scope.lastErr = errWait
			}
			return nil, nil, resolveMixedPickError(scope.lastErr, errPick)
		}
		if errModeration := s.manager.moderateRequest(candidate.execCtx, candidate.auth, scope.opts); errModeration != nil {
			return nil, nil, errModeration
		}

		releaseSlot, errSlot := s.manager.acquireAccountSlot(candidate.auth)
		if errSlot == nil {
			return candidate, releaseSlot, nil
		}
		if isRequestInvalidError(errSlot) {
			return nil, nil, errSlot
		}
		scope.saturated = append(scope.saturated, candidate)
		scope.lastErr = errSlot
	}
}

// waitForSaturatedSlot queues on every candidate that was skipped for being at its
// concurrency limit and resumes with whichever frees a slot first.
func (s executionService) waitForSaturatedSlot(ctx context.Context, scope *mixedExecutionScope) (*mixedExecutionCandidate, func(), error) {
	if len(scope.saturated) == 0 {
		return nil, nil, errNoSaturatedCandidate
	}
	auths := make([]*Auth, 0, len(scope.saturated))
	for _, candidate := range scope.saturated {
		auths = append(auths, candidate.auth)
	}
	releaseSlot, authID, err := s.manager.waitAccountSlot(ctx, auths)
	if err != nil {
		return nil, nil, err
	}
	for i, candidate := range scope.saturated {
		if candidate.auth.ID != authID {
			continue
		}
		// Consume the entry: an account that has had its turn must not be queued
		// for again, otherwise a candidate whose request keeps failing would loop
		// through the queue instead of failing over or giving up.
		scope.saturated = append(scope.saturated[:i], scope.saturated[i+1:]...)
		return candidate, releaseSlot, nil
	}
	// Unreachable in practice; release rather than leak the slot we were granted.
	releaseSlot()
	return nil, nil, errNoSaturatedCandidate
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
	sourceFormat sdktranslator.Format,
	streamResult *cliproxyexecutor.StreamResult,
	releaseSlot func(),
) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	streamHeaders := streamResult.Headers.Clone()
	go func(streamCtx context.Context, streamAuth *Auth, streamProvider string, streamChunks <-chan cliproxyexecutor.StreamChunk, headers http.Header) {
		defer close(out)
		if releaseSlot != nil {
			defer releaseSlot()
		}
		var failed bool
		forward := true
		completionTracker := newResponsesStreamCompletionTracker(sourceFormat)
		for chunk := range streamChunks {
			if chunk.Err == nil && completionTracker != nil && len(chunk.Payload) > 0 {
				completionTracker.Observe(chunk.Payload)
			}
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
		if !failed && completionTracker != nil {
			if err := completionTracker.ErrIfIncomplete(); err != nil {
				failed = true
				rerr := errorFromExecution(err)
				s.manager.MarkResult(streamCtx, Result{
					AuthID:     streamAuth.ID,
					Provider:   streamProvider,
					Model:      routeModel,
					Success:    false,
					Error:      rerr,
					RetryAfter: retryAfterFromError(err),
					Headers:    headers.Clone(),
				})
				if forward {
					chunk := cliproxyexecutor.StreamChunk{Err: err}
					if streamCtx == nil {
						out <- chunk
					} else {
						select {
						case <-streamCtx.Done():
						case out <- chunk:
						}
					}
				}
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
