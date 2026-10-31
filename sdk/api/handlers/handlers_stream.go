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
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return nil, nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
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
		Headers:         requestExecutionHeaders(ctx),
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
		return nil, nil, nil, &StreamErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	var upstreamHeaders http.Header
	if passthroughHeadersEnabled {
		upstreamHeaders = cloneHeader(FilterUpstreamHeaders(streamResult.Headers))
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
	}
	if opts.Headers.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion && streamResult.Headers.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion {
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
		upstreamHeaders.Set(sdktranslator.CompletedToolBlocksHeader, sdktranslator.CompletedToolBlocksVersion)
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *StreamErrorMessage, 1)
	// Capture retry budget before the goroutine so the no-retry path does not close
	// over req/opts (those hold multi-MB inbound bodies for the whole SSE lifetime).
	maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)
	if maxBootstrapRetries <= 0 {
		go forwardStreamWithoutBootstrapRetry(ctx, handlerType, chunks, dataChan, errChan)
	} else {
		go forwardStreamWithBootstrapRetry(
			ctx,
			h,
			handlerType,
			providers,
			req,
			opts,
			passthroughHeadersEnabled,
			upstreamHeaders,
			chunks,
			dataChan,
			errChan,
			maxBootstrapRetries,
		)
	}
	return dataChan, upstreamHeaders, errChan, nil
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
	h *BaseAPIHandler,
	handlerType string,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	passthroughHeadersEnabled bool,
	upstreamHeaders http.Header,
	chunks <-chan coreexecutor.StreamChunk,
	dataChan chan<- []byte,
	errChan chan<- *StreamErrorMessage,
	maxBootstrapRetries int,
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
				// retry a few times (to allow auth rotation / transient recovery).
				if !sentPayload && bootstrapRetries < maxBootstrapRetries && bootstrapEligibleStreamErr(streamErr) {
					bootstrapRetries++
					retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
					if retryErr == nil {
						if passthroughHeadersEnabled {
							replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
						}
						upstreamHeaders.Del(sdktranslator.CompletedToolBlocksHeader)
						if upstreamHeaders != nil && opts.Headers.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion && retryResult.Headers.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion {
							upstreamHeaders.Set(sdktranslator.CompletedToolBlocksHeader, sdktranslator.CompletedToolBlocksVersion)
						}
						chunks = retryResult.Chunks
						continue outer
					}
					streamErr = retryErr
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
