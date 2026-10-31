package handlers

import (
	"context"
	"errors"
	"net/http"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *StreamErrorMessage) {
	dataChan, upstreamHeaders, errChan, startErr := h.StartStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt)
	if startErr == nil {
		return dataChan, upstreamHeaders, errChan
	}
	legacyErrChan := make(chan *StreamErrorMessage, 1)
	legacyErrChan <- startErr
	close(legacyErrChan)
	return nil, nil, legacyErrChan
}

// StartStreamWithAuthManager starts a streaming request via the core auth manager.
// Errors returned directly from this method happen before any downstream stream
// bytes are written, so HTTP handlers can still return a normal JSON error.
func (h *BaseAPIHandler) StartStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *StreamErrorMessage, *StreamErrorMessage) {
	ctx, waits := h.startStreamWaits(ctx, alt)
	if plan := virtualModelPlanFor(ctx, modelName); plan != nil {
		return h.startVirtualModelStream(ctx, waits, plan, handlerType, rawJSON, alt)
	}
	stream, errMsg := h.beginStream(ctx, handlerType, modelName, rawJSON, alt)
	if committed := waits.handOver(); errMsg != nil {
		if committed {
			return failedAfterCommit(errMsg)
		}
		return nil, nil, nil, errMsg
	}
	dataChan, upstreamHeaders, errChan := h.forwardStream(handlerType, stream, nil)
	return dataChan, upstreamHeaders, errChan, nil
}

// startedStream is a stream the scheduler handed over: the context it runs
// under, and what a bootstrap retry needs to start it again.
type startedStream struct {
	ctx    context.Context
	result *coreexecutor.StreamResult
	retry  *streamRetryState
}

// beginStream asks the scheduler for a stream of modelName. ctx carries the
// waits of startStreamWaits; once they are handed over (a virtual model's next
// member, started from the forwarding goroutine) the scheduler waits silently.
func (h *BaseAPIHandler) beginStream(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (*startedStream, *StreamErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return nil, errMsg
	}
	reqMeta := executionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          true,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = 499 // Client Closed Request
		} else if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, &StreamErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	return &startedStream{
		ctx:    ctx,
		result: streamResult,
		retry:  &streamRetryState{providers: providers, req: req, opts: opts},
	}, nil
}

// forwardStream hands a started stream to a goroutine that forwards it to the
// protocol handler, retrying on another account while nothing has reached the
// client. next, when set, moves a virtual model on to its next member once
// those retries are used up.
func (h *BaseAPIHandler) forwardStream(handlerType string, stream *startedStream, next streamNextMember) (<-chan []byte, http.Header, <-chan *StreamErrorMessage) {
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	var upstreamHeaders http.Header
	if passthroughHeadersEnabled {
		upstreamHeaders = cloneHeader(FilterUpstreamHeaders(stream.result.Headers))
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
	}
	chunks := stream.result.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *StreamErrorMessage, 1)
	maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)
	if maxBootstrapRetries <= 0 && next == nil {
		// The no-retry path does not close over the request (see streamRetryState).
		go forwardStreamWithoutBootstrapRetry(stream.ctx, handlerType, chunks, dataChan, errChan)
	} else {
		go forwardStreamWithBootstrapRetry(
			stream.ctx,
			handlerType,
			stream.retry,
			passthroughHeadersEnabled,
			upstreamHeaders,
			chunks,
			dataChan,
			errChan,
			max(maxBootstrapRetries, 0),
			newStreamFailover(h, stream.result),
			next,
		)
	}
	return dataChan, upstreamHeaders, errChan
}

// streamNextMember starts the next member of a virtual model after cause ended
// the current one before its first byte. It returns the member's stream, or
// the error to report when no member is left to take over.
type streamNextMember func(cause error) (*startedStream, *StreamErrorMessage)

// streamRetryState is what a bootstrap retry needs to start the request again.
// req and opts carry the inbound body (several MB for a large prompt) twice
// over, so the forwarding goroutine reaches them only through this pointer and
// lets go of it with the first payload, after which no retry is possible:
// a stream that runs for minutes must not keep its request alive all along.
type streamRetryState struct {
	providers []string
	req       coreexecutor.Request
	opts      coreexecutor.Options
}

func forwardStreamWithoutBootstrapRetry(
	ctx context.Context,
	handlerType string,
	chunks <-chan coreexecutor.StreamChunk,
	dataChan chan<- []byte,
	errChan chan<- *StreamErrorMessage,
) {
	defer close(dataChan)
	defer close(errChan)
	for {
		chunk, ok := recvStreamChunk(ctx, chunks)
		if !ok {
			return
		}
		if chunk.Err != nil {
			_ = sendStreamErr(ctx, errChan, streamErrorMessageFromErr(chunk.Err))
			return
		}
		if len(chunk.Payload) == 0 {
			continue
		}
		if handlerType == "openai-response" {
			if err := validateSSEDataJSON(chunk.Payload); err != nil {
				_ = sendStreamErr(ctx, errChan, &StreamErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
				return
			}
		}
		if !sendStreamData(ctx, dataChan, cloneBytes(chunk.Payload)) {
			return
		}
	}
}

func forwardStreamWithBootstrapRetry(
	ctx context.Context,
	handlerType string,
	retry *streamRetryState,
	passthroughHeadersEnabled bool,
	upstreamHeaders http.Header,
	chunks <-chan coreexecutor.StreamChunk,
	dataChan chan<- []byte,
	errChan chan<- *StreamErrorMessage,
	maxBootstrapRetries int,
	failover *streamFailover,
	next streamNextMember,
) {
	defer close(dataChan)
	defer close(errChan)
	sentPayload := false
	bootstrapRetries := 0

outer:
	for {
		for {
			chunk, ok := recvStreamChunk(ctx, chunks)
			if !ok {
				return
			}
			if chunk.Err != nil {
				streamErr := chunk.Err
				// Safe bootstrap recovery: if the upstream fails before any payload bytes are sent,
				// retry on another account (the failover excludes every account that already
				// failed this request, so the candidate count bounds the retries too).
				if !sentPayload && bootstrapRetries < maxBootstrapRetries && bootstrapEligibleStreamErr(streamErr) {
					bootstrapRetries++
					retryResult, retryErr := failover.retry(ctx, retry.providers, retry.req, retry.opts, streamErr, bootstrapRetries, maxBootstrapRetries)
					if retryErr == nil {
						if passthroughHeadersEnabled {
							replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
						}
						chunks = retryResult.Chunks
						continue outer
					}
					streamErr = retryErr
				}
				// A virtual model turns to its next member while nothing has
				// reached the client; that member gets its own retries.
				if !sentPayload && next != nil {
					member, msg := next(streamErr)
					if member == nil {
						_ = sendStreamErr(ctx, errChan, msg)
						return
					}
					if passthroughHeadersEnabled {
						replaceHeader(upstreamHeaders, FilterUpstreamHeaders(member.result.Headers))
					}
					ctx, retry, chunks = member.ctx, member.retry, member.result.Chunks
					failover = newStreamFailover(failover.h, member.result)
					bootstrapRetries = 0
					continue outer
				}
				_ = sendStreamErr(ctx, errChan, streamErrorMessageFromErr(streamErr))
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			if handlerType == "openai-response" {
				if err := validateSSEDataJSON(chunk.Payload); err != nil {
					_ = sendStreamErr(ctx, errChan, &StreamErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
					return
				}
			}
			sentPayload = true
			// Past the first byte there is no retry, so nothing needs the request
			// any more (see streamRetryState).
			retry, next = nil, nil
			if !sendStreamData(ctx, dataChan, cloneBytes(chunk.Payload)) {
				return
			}
		}
	}
}

func recvStreamChunk(ctx context.Context, chunks <-chan coreexecutor.StreamChunk) (coreexecutor.StreamChunk, bool) {
	if ctx == nil {
		chunk, ok := <-chunks
		return chunk, ok
	}
	select {
	case <-ctx.Done():
		return coreexecutor.StreamChunk{}, false
	case chunk, ok := <-chunks:
		return chunk, ok
	}
}

func sendStreamErr(ctx context.Context, errChan chan<- *StreamErrorMessage, msg *StreamErrorMessage) bool {
	if ctx == nil {
		errChan <- msg
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case errChan <- msg:
		return true
	}
}

func sendStreamData(ctx context.Context, dataChan chan<- []byte, chunk []byte) bool {
	if ctx == nil {
		dataChan <- chunk
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case dataChan <- chunk:
		return true
	}
}

func bootstrapEligibleStreamErr(err error) bool {
	status := statusFromError(err)
	if status == 0 {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
		http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return status >= http.StatusInternalServerError
	}
}

func streamErrorMessageFromErr(streamErr error) *StreamErrorMessage {
	status := http.StatusInternalServerError
	if se, ok := streamErr.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			status = code
		}
	}
	var addon http.Header
	if he, ok := streamErr.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &StreamErrorMessage{StatusCode: status, Error: streamErr, Addon: addon}
}
