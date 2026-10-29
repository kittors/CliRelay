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
// It owns three things that must agree about what was actually sent:
//
//   - compression: the body leaves as zstd when the ChatGPT backend expects it
//     (codex_request_zstd.go), while the request log keeps the plain JSON;
//   - the log entry itself, written here so it carries the final headers
//     (Content-Encoding included) and the body exactly as built, including the
//     prompt_cache_key the request helper added;
//   - the one retry after `invalid_encrypted_content`: when the upstream refuses
//     replayed reasoning it cannot read, the request is sent again without that
//     reasoning instead of failing a turn the model could have answered.
//
// fallbackBody is logged when the request body cannot be read back.
func (e *CodexExecutor) doCodexResponses(execCtx *ExecutionContext, recorder UpstreamRecorder, httpReq *http.Request, fallbackBody []byte) (*http.Response, error) {
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

	peek, err := io.ReadAll(io.LimitReader(resp.Body, codexErrorPeekBytes))
	// Hand the caller the body it would have had, whatever happens next: the
	// peeked prefix followed by whatever was not read.
	resp.Body = &codexPeekedBody{Reader: io.MultiReader(bytes.NewReader(peek), resp.Body), closer: resp.Body}
	if err != nil || !bytes.Contains(peek, codexInvalidCiphertextCode) {
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
	return httpClient.Do(retry) //nolint:bodyclose // returned to the caller, who owns closing it.
}

// codexPeekedBody serves a response body that has been partly read ahead.
type codexPeekedBody struct {
	io.Reader
	closer io.Closer
}

func (b *codexPeekedBody) Close() error { return b.closer.Close() }
