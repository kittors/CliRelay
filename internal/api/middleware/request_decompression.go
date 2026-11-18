package middleware

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
)

type decompressedReadCloser struct {
	io.Reader
	closeFn func() error
}

func (r *decompressedReadCloser) Close() error {
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}

// DecompressRequestMiddleware normalizes compressed API request bodies before
// downstream middlewares inspect them. Limits are enforced on both the inbound
// compressed stream and the decoded stream to prevent oversized requests and
// decompression bombs.
func DecompressRequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Method == http.MethodGet {
			c.Next()
			return
		}

		limit := bodyutil.ModelRequestBodyLimit()
		origBody := c.Request.Body
		rawBody := http.MaxBytesReader(c.Writer, origBody, limit)
		br := bufio.NewReader(rawBody)

		encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding")))
		if encoding == "" || encoding == "identity" {
			if peek, _ := br.Peek(4); len(peek) >= 2 && peek[0] == 0x1f && peek[1] == 0x8b {
				encoding = "gzip"
			} else if len(peek) >= 4 && peek[0] == 0x28 && peek[1] == 0xb5 && peek[2] == 0x2f && peek[3] == 0xfd {
				encoding = "zstd"
			}
		}

		wrapDecoded := func(body io.ReadCloser) io.ReadCloser {
			return http.MaxBytesReader(c.Writer, body, limit)
		}
		clearDecodedHeaders := func() {
			c.Request.Header.Del("Content-Encoding")
			c.Request.Header.Del("Content-Length")
			c.Request.ContentLength = -1
		}
		abortInvalidEncoding := func(message string, status int) {
			_ = rawBody.Close()
			c.AbortWithStatusJSON(status, gin.H{
				"error": gin.H{
					"message": message,
					"type":    "invalid_request_error",
				},
			})
		}

		switch encoding {
		case "", "identity":
			c.Request.Body = &decompressedReadCloser{
				Reader: br,
				closeFn: func() error {
					return rawBody.Close()
				},
			}
		case "gzip", "x-gzip":
			reader, err := gzip.NewReader(br)
			if err != nil {
				abortInvalidEncoding("invalid gzip request body", http.StatusBadRequest)
				return
			}
			c.Request.Body = wrapDecoded(&decompressedReadCloser{
				Reader: reader,
				closeFn: func() error {
					_ = reader.Close()
					return rawBody.Close()
				},
			})
			clearDecodedHeaders()
		case "br":
			c.Request.Body = wrapDecoded(&decompressedReadCloser{
				Reader: brotli.NewReader(br),
				closeFn: func() error {
					return rawBody.Close()
				},
			})
			clearDecodedHeaders()
		case "zstd":
			reader, err := zstd.NewReader(br)
			if err != nil {
				abortInvalidEncoding("invalid zstd request body", http.StatusBadRequest)
				return
			}
			c.Request.Body = wrapDecoded(&decompressedReadCloser{
				Reader: reader,
				closeFn: func() error {
					reader.Close()
					return rawBody.Close()
				},
			})
			clearDecodedHeaders()
		case "deflate":
			reader, err := zlib.NewReader(br)
			if err != nil {
				abortInvalidEncoding("invalid deflate request body", http.StatusBadRequest)
				return
			}
			c.Request.Body = wrapDecoded(&decompressedReadCloser{
				Reader: reader,
				closeFn: func() error {
					_ = reader.Close()
					return rawBody.Close()
				},
			})
			clearDecodedHeaders()
		default:
			abortInvalidEncoding("unsupported content encoding: "+encoding, http.StatusUnsupportedMediaType)
			return
		}

		c.Next()
	}
}
