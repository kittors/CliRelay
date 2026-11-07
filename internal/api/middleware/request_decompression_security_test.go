// The invalidZstdHuffmanFrames fixture below is adapted from the upstream zstd tests.
// Original source-file notice:
// Copyright 2019+ Klaus Post. All rights reserved.
// Based on work by Yann Collet, released under BSD License.
// The following upstream license applies to that adapted fixture.
/*
Copyright (c) 2012 The Go Authors. All rights reserved.
Copyright (c) 2019 Klaus Post. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
)

// These bounded frames reproduce GO-2026-6865. The malformed Huffman weights
// previously panicked in the streaming decoder's goroutine, outside HTTP recovery.
// Fixture source: github.com/klauspost/compress, commit 7f5d8528720c895a6106f201719f87eb030a565d,
// TestDecoderHuffWeightsTableLog (BSD-3-Clause).
func invalidZstdHuffmanFrames() (oneFrame, twoFrames []byte) {
	oneFrame = slices.Concat(
		[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38, 0x74, 0x06, 0x00, 0x42, 0xb8, 0x32, 0x0d, 0x1a, 0x00, 0x00, 0x00, 0x90, 0xaf, 0xfa, 0xff, 0x03, 0x00, 0x80, 0xff, 0x7f},
		bytes.Repeat([]byte{0xa5, 0x94, 0x52, 0x4a, 0x29}, 37),
		[]byte{0xa5, 0x94, 0x12, 0x00, 0xad, 0x56, 0x03, 0xee, 0xff, 0xdf, 0xb3, 0x1a, 0x1c, 0x1a, 0x00, 0xe0, 0xff, 0x1f, 0x0f, 0xb1, 0x9c, 0x5a, 0xcf, 0x4d, 0xe4, 0x1d, 0x7b, 0xab, 0x99, 0x5e, 0x01, 0x51, 0xa4, 0x73, 0xd8, 0xb1, 0xf3, 0x39, 0x4b, 0x67, 0xf6, 0xab, 0x1a, 0xab, 0x1a, 0xab, 0x1a},
		bytes.Repeat([]byte{0x8c, 0x31, 0xc6, 0x18, 0x63}, 1365),
		[]byte{0x8c, 0x31},
		bytes.Repeat([]byte{0x63, 0x8c, 0x31, 0xc6, 0x18}, 1365),
		[]byte{0x63, 0x2c},
		bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365),
		[]byte{0x31, 0x46},
		bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365),
		[]byte{0x31, 0x06, 0x00},
	)
	// Mark the first block last, then put the second block in a separate frame.
	twoFrames = slices.Concat(oneFrame[:6], []byte{0x75, 0x06, 0x00}, oneFrame[9:215], oneFrame[:6], oneFrame[215:])
	return oneFrame, twoFrames
}

func TestDecompressRequestMiddlewareRejectsInvalidZstdHuffmanTables(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(1 << 20)

	r := gin.New()
	r.Use(DecompressRequestMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		defer func() { _ = c.Request.Body.Close() }()
		decoded, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Data(http.StatusOK, "application/json", decoded)
	})

	oneFrame, twoFrames := invalidZstdHuffmanFrames()
	valid := []byte(`{"input":"still healthy"}`)
	validCompressed := zstdRequestBody(t, valid)
	for _, fixture := range []struct {
		name string
		body []byte
	}{{"one_frame", oneFrame}, {"two_frames", twoFrames}} {
		for _, encoding := range []string{"zstd", ""} {
			t.Run(fmt.Sprintf("%s/encoding=%q", fixture.name, encoding), func(t *testing.T) {
				// Exercise repeated failures and pooled decoder state, then a valid request.
				for round := 0; round < 2; round++ {
					for _, request := range []struct {
						body []byte
						want int
					}{{fixture.body, http.StatusBadRequest}, {validCompressed, http.StatusOK}} {
						req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request.body))
						req.Header.Set("Content-Encoding", encoding)
						w := httptest.NewRecorder()
						r.ServeHTTP(w, req)
						if w.Code != request.want {
							t.Fatalf("round %d: status = %d, want %d", round, w.Code, request.want)
						}
						if request.want == http.StatusOK && !bytes.Equal(w.Body.Bytes(), valid) {
							t.Fatalf("valid request after malformed frame: got %q", w.Body.Bytes())
						}
					}
				}
			})
		}
	}
}

func TestDecompressRequestMiddlewareCompressionBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	const limit = 1024
	bodyutil.SetModelRequestBodyLimit(limit)

	for _, codec := range []struct {
		name   string
		encode func(*testing.T, []byte) []byte
	}{{"br", brotliRequestBody}, {"zstd", zstdRequestBody}} {
		for _, size := range []int{limit - 1, limit, limit + 1} {
			t.Run(fmt.Sprintf("%s/decoded=%d", codec.name, size), func(t *testing.T) {
				raw := bytes.Repeat([]byte("a"), size)
				r := gin.New()
				r.Use(DecompressRequestMiddleware())
				r.POST("/v1/responses", func(c *gin.Context) {
					defer func() { _ = c.Request.Body.Close() }()
					decoded, err := io.ReadAll(c.Request.Body)
					if size > limit {
						if !bodyutil.IsTooLarge(err) {
							t.Fatalf("oversized body: got error %v", err)
						}
						c.Status(http.StatusRequestEntityTooLarge)
						return
					}
					if err != nil || !bytes.Equal(decoded, raw) {
						t.Fatalf("decoded body mismatch: len=%d, err=%v", len(decoded), err)
					}
					c.Status(http.StatusNoContent)
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(codec.encode(t, raw)))
				req.Header.Set("Content-Encoding", codec.name)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				want := http.StatusNoContent
				if size > limit {
					want = http.StatusRequestEntityTooLarge
				}
				if w.Code != want {
					t.Fatalf("status = %d, want %d", w.Code, want)
				}
			})
		}
	}
}

func TestDecompressRequestMiddlewareRejectsMalformedCompressedBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(1 << 20)

	for _, codec := range []struct {
		name   string
		encode func(*testing.T, []byte) []byte
	}{{"br", brotliRequestBody}, {"zstd", zstdRequestBody}} {
		complete := codec.encode(t, []byte(`{"input":"complete body"}`))
		for _, input := range []struct {
			name string
			body []byte
		}{{"corrupt", bytes.Repeat([]byte{0xff}, 16)}, {"truncated", complete[:len(complete)-1]}} {
			t.Run(codec.name+"/"+input.name, func(t *testing.T) {
				r := gin.New()
				r.Use(DecompressRequestMiddleware())
				r.POST("/v1/responses", func(c *gin.Context) {
					defer func() { _ = c.Request.Body.Close() }()
					if _, err := io.ReadAll(c.Request.Body); err == nil {
						t.Fatal("malformed compressed body was accepted")
					}
					c.Status(http.StatusBadRequest)
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(input.body))
				req.Header.Set("Content-Encoding", codec.name)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
				}
			})
		}
	}
}
