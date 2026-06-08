package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type timeoutReadCloser struct{}

func (timeoutReadCloser) Read([]byte) (int, error) {
	return 0, timeoutReadError{}
}

func (timeoutReadCloser) Close() error { return nil }

type timeoutReadError struct{}

func (timeoutReadError) Error() string {
	return "read tcp 127.0.0.1:8317->127.0.0.1:60272: i/o timeout"
}
func (timeoutReadError) Timeout() bool   { return true }
func (timeoutReadError) Temporary() bool { return true }

func TestReadJSONRequestBodyReturnsTooLargeError(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	oversized := bytes.Repeat([]byte("a"), (16<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	body, ok := ReadJSONRequestBody(c)
	if ok {
		t.Fatalf("expected oversized request body to fail, got ok")
	}
	if body != nil {
		t.Fatalf("expected no body on failure")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, recorder.Code)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if payload.Error.Code != "request_body_too_large" {
		t.Fatalf("expected request_body_too_large code, got %q", payload.Error.Code)
	}
}

func TestReadJSONRequestBodyReturnsTimeoutError(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = timeoutReadCloser{}
	c.Request = req

	body, ok := ReadJSONRequestBody(c)
	if ok {
		t.Fatalf("expected timeout request body to fail, got ok")
	}
	if body != nil {
		t.Fatalf("expected no body on failure")
	}
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("expected status %d, got %d", http.StatusRequestTimeout, recorder.Code)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if payload.Error.Type != "request_timeout" {
		t.Fatalf("expected request_timeout type, got %q", payload.Error.Type)
	}
	if payload.Error.Code != "request_timeout" {
		t.Fatalf("expected request_timeout code, got %q", payload.Error.Code)
	}
	if payload.Error.Message != "Request timed out while reading the request body" {
		t.Fatalf("unexpected timeout message: %q", payload.Error.Message)
	}
}

func TestReadJSONRequestBodyRestoresRequestBody(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	body, ok := ReadJSONRequestBody(c)
	if !ok {
		t.Fatalf("expected request body to be readable")
	}
	if string(body) != `{"model":"gpt-4.1"}` {
		t.Fatalf("unexpected body: %s", string(body))
	}

	bodyAgain, ok := ReadJSONRequestBody(c)
	if !ok {
		t.Fatalf("expected restored request body to be reusable")
	}
	if string(bodyAgain) != string(body) {
		t.Fatalf("expected restored body %q, got %q", string(body), string(bodyAgain))
	}
}

func TestTimeoutReadErrorImplementsReaderContract(t *testing.T) {
	t.Parallel()

	var rc io.ReadCloser = timeoutReadCloser{}
	_, err := rc.Read(make([]byte, 8))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected timeout-capable error, got %v", err)
	}
}
