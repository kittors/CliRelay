package executor

import (
	"bytes"
	"io"
	"net/http"
)

// codexInvalidCiphertextCode is the error code the upstream answers with when a
// replayed reasoning item cannot be decrypted. It is matched as a substring of
// the error body so any envelope the backend wraps it in is recognised.
var codexInvalidCiphertextCode = []byte("invalid_encrypted_content")

// codexErrorPeekBytes bounds how much of a 400 body is read to look for the code.
// The code sits at the start of the error object.
const codexErrorPeekBytes = 64 << 10

// doCodexResponses sends one prepared POST to the Codex upstream.
//
// It owns four things that must agree about what was actually sent:
//
//   - compression: the body leaves as zstd when the ChatGPT backend expects it
//     (codex_request_zstd.go), while the request log keeps the plain JSON;
//   - the log entry itself, written here so it carries the final headers
//     (Content-Encoding included) and the body exactly as built, including the
//     prompt_cache_key the request helper added;
//   - the one retry after `invalid_encrypted_content`: when the upstream refuses
//     replayed reasoning it cannot read, the request is sent again without that
//     reasoning instead of failing a turn the model could have answered;
//   - the shape of a request refused with a 400 that names no error code, which
//     is kept for diagnosis because the body itself is not (codex_request_shape.go).
//
// fallbackBody is logged when the request body cannot be read back.
//
// Whatever comes back, the quota windows it reports are passed on to the
// scheduler (codex_rate_limits.go). That is done here, once, for every kind of
// Responses call, and only for the answer the caller actually receives.
func (e *CodexExecutor) doCodexResponses(execCtx *ExecutionContext, recorder UpstreamRecorder, httpReq *http.Request, fallbackBody []byte) (*http.Response, error) {
	resp, err := e.sendCodexResponses(execCtx, recorder, httpReq, fallbackBody)
	if err == nil {
		observeCodexRateLimits(execCtx.Context, execCtx.Auth, httpReq.URL, resp)
	}
	return resp, err
}

func (e *CodexExecutor) sendCodexResponses(execCtx *ExecutionContext, recorder UpstreamRecorder, httpReq *http.Request, fallbackBody []byte) (*http.Response, error) {
	auth := execCtx.Auth
	plain := codexRequestPlainBody(httpReq)
	if plain == nil {
		plain = fallbackBody
	}
	compressCodexRequest(e.cfg, auth, httpReq, plain)
	recorder.RecordRequest(httpReq.URL.String(), httpReq.Method, httpReq.Header.Clone(), plain)

	httpClient := execCtx.HTTPClient(0)
	resp, err := httpClient.Do(httpReq) //nolint:bodyclose // returned to the caller, who owns closing it.
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, err
	}

	peek, err := peekCodexErrorBody(resp)
	if err != nil || !bytes.Contains(peek, codexInvalidCiphertextCode) {
		if err == nil {
			// A 400 that names no error code explains nothing, so the shape of what
			// was sent is the only evidence there will be of why it was refused.
			recordCodexUnexplainedBadRequest(execCtx, httpReq, plain, peek, false)
		}
		return resp, nil
	}
	stripped, dropped := dropCodexReasoningItems(plain, anyCodexCiphertext)
	if dropped == 0 {
		return resp, nil
	}
	logWithRequestID(execCtx.Context).Infof("codex executor: upstream rejected replayed reasoning ciphertext (invalid_encrypted_content); retrying once without %d reasoning item(s)", dropped)
	// Keep the refused attempt in the request log with its answer, so the log
	// shows why one request went out twice.
	recorder.RecordResponseMetadata(resp.StatusCode, resp.Header.Clone())
	recorder.AppendResponseChunk(peek)
	_ = resp.Body.Close()

	retry := httpReq.Clone(httpReq.Context())
	retry.Header.Del("Content-Encoding")
	setCodexRequestBody(retry, stripped)
	compressCodexRequest(e.cfg, auth, retry, stripped)
	recorder.RecordRequest(retry.URL.String(), retry.Method, retry.Header.Clone(), stripped)
	retryResp, err := httpClient.Do(retry) //nolint:bodyclose // returned to the caller, who owns closing it.
	if err != nil || retryResp == nil || retryResp.StatusCode != http.StatusBadRequest {
		return retryResp, err
	}
	// Refused again, and this answer is the one the attempt ends with: its shape
	// is the body that went out without the reasoning, marked as such.
	if retryPeek, errPeek := peekCodexErrorBody(retryResp); errPeek == nil {
		recordCodexUnexplainedBadRequest(execCtx, retry, stripped, retryPeek, true)
	}
	return retryResp, nil
}

// peekCodexErrorBody reads the start of a 400 body and puts it back, so the
// caller still gets the body it would have had whatever is done with the prefix:
// the peeked bytes followed by whatever was not read.
func peekCodexErrorBody(resp *http.Response) ([]byte, error) {
	peek, err := io.ReadAll(io.LimitReader(resp.Body, codexErrorPeekBytes))
	resp.Body = &codexPeekedBody{Reader: io.MultiReader(bytes.NewReader(peek), resp.Body), closer: resp.Body}
	return peek, err
}

// codexPeekedBody serves a response body that has been partly read ahead.
type codexPeekedBody struct {
	io.Reader
	closer io.Closer
}

func (b *codexPeekedBody) Close() error { return b.closer.Close() }
