package executor

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Codex request compression.
//
// The official Codex client compresses the body of POST /responses with zstd
// (level 3) whenever it talks to a ChatGPT account, and says so with
// Content-Encoding: zstd. Sending the bytes the client it presents itself as
// would send is the point; the practical gain is that large histories are
// accepted, where a 1.1 MB plain body was reported to draw a 503 and the same
// request compressed a 200.
//
// Only that exact case is compressed. API-key accounts and custom base URLs talk
// to servers whose support for request decompression is unknown, /responses/compact
// is deliberately left alone, and the websocket transport has no request body to
// compress. Anything outside the ChatGPT backend therefore keeps plain JSON.

const (
	codexChatGPTHost = "chatgpt.com"

	// codexZstdWindowSize matches the 2 MiB window zstd level 3 uses, which also
	// keeps each pooled encoder small enough to hold a few of them for bursts of
	// concurrent multi-megabyte histories.
	codexZstdWindowSize = 2 << 20
	// codexZstdEncoderConcurrency bounds the pooled encoders EncodeAll may run at
	// once. Compression is a few milliseconds per megabyte, so a request that has
	// to wait for an encoder waits far less than the upstream call it precedes.
	codexZstdEncoderConcurrency = 4
)

var codexZstdEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil,
		// SpeedDefault is zstd's level 3.
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(codexZstdWindowSize),
		zstd.WithEncoderConcurrency(codexZstdEncoderConcurrency),
		// The Rust zstd crate the official client uses writes no frame checksum
		// unless asked to.
		zstd.WithEncoderCRC(false),
	)
})

// encodeCodexZstd compresses plain into a single zstd frame.
func encodeCodexZstd(plain []byte) ([]byte, bool) {
	enc, err := codexZstdEncoder()
	if err != nil || enc == nil {
		return nil, false
	}
	// JSON histories compress well beyond 4:1; the estimate only avoids regrowth.
	return enc.EncodeAll(plain, make([]byte, 0, len(plain)/4+64)), true
}

// isCodexChatGPTBackend reports whether auth is a ChatGPT OAuth account talking
// to the ChatGPT backend. Everything chatgpt.com-specific in the Codex executor
// (request compression, the format of reasoning ciphertext) is keyed off this one
// predicate so the two cannot disagree about which accounts they apply to.
func isCodexChatGPTBackend(auth *cliproxyauth.Auth, target *url.URL) bool {
	if !isCodexOAuthAdmissionAuth(auth) || target == nil {
		return false
	}
	return strings.EqualFold(target.Hostname(), codexChatGPTHost)
}

func isCodexChatGPTBackendBaseURL(auth *cliproxyauth.Auth, baseURL string) bool {
	if !isCodexOAuthAdmissionAuth(auth) {
		return false
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		// codexCreds leaves the base URL empty for OAuth accounts and the executor
		// then falls back to the ChatGPT backend.
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return isCodexChatGPTBackend(auth, parsed)
}

// codexRequestCompressionApplies reports whether req should leave zstd-compressed.
func codexRequestCompressionApplies(cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request) bool {
	if req == nil || req.Method != http.MethodPost || req.URL == nil {
		return false
	}
	if !cfg.CodexRequestZstdEnabled() || !isCodexChatGPTBackend(auth, req.URL) {
		return false
	}
	// A client-supplied encoding would be applied twice; there is none today, but
	// the check keeps this correct if a header ever reaches here.
	if strings.TrimSpace(req.Header.Get("Content-Encoding")) != "" {
		return false
	}
	path := strings.TrimRight(req.URL.Path, "/")
	return strings.HasSuffix(path, "/responses")
}

// codexRequestPlainBody returns the bytes req would send, without consuming it.
func codexRequestPlainBody(req *http.Request) []byte {
	if req == nil || req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil || body == nil {
		return nil
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil
	}
	return data
}

// setCodexRequestBody replaces the body of req, keeping Content-Length and the
// replay hook (HTTP/2 and connection-reuse retries need it) consistent with it.
func setCodexRequestBody(req *http.Request, data []byte) {
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

// compressCodexRequest swaps req's body for its zstd form when that is what the
// official client would send. plain is the uncompressed body; callers keep it for
// request logs, which must stay readable.
func compressCodexRequest(cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request, plain []byte) bool {
	if len(plain) == 0 || !codexRequestCompressionApplies(cfg, auth, req) {
		return false
	}
	compressed, ok := encodeCodexZstd(plain)
	if !ok {
		return false
	}
	setCodexRequestBody(req, compressed)
	req.Header.Set("Content-Encoding", "zstd")
	return true
}
