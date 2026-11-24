package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
)

func TestWriteInvalidBodyErrorDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("empty body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeInvalidBodyError(c, io.EOF)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != "empty_request_body" || !strings.Contains(env.Error.Message, "为空") {
			t.Fatalf("unexpected envelope: %+v", env)
		}
	})

	t.Run("syntax error reports offset", func(t *testing.T) {
		var dst map[string]any
		err := json.Unmarshal([]byte(`{"name": }`), &dst)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeInvalidBodyError(c, err)
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != "invalid_json_syntax" || !strings.Contains(env.Error.Message, "语法有误") {
			t.Fatalf("unexpected envelope: %+v", env)
		}
		if _, ok := env.Error.Details["offset"]; !ok {
			t.Fatalf("expected offset detail, got %#v", env.Error.Details)
		}
	})

	t.Run("field type mismatch names field and expected type", func(t *testing.T) {
		var dst struct {
			Limit int `json:"limit"`
		}
		err := json.Unmarshal([]byte(`{"limit":"ten"}`), &dst)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeInvalidBodyError(c, err)
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != "invalid_field_type" || !strings.Contains(env.Error.Message, "limit") || !strings.Contains(env.Error.Message, "整数") {
			t.Fatalf("unexpected envelope: %+v", env)
		}
		if env.Error.Details["field"] != "limit" {
			t.Fatalf("details.field = %v, want limit", env.Error.Details["field"])
		}
	})
}

func TestWriteInternalErrorDoesNotLeakUnderlyingDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/demo", nil)
	writeInternalError(c, "保存模型定价", errors.New("pq: password authentication failed for user cliproxy /etc/secret.db"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "internal_error" || !strings.Contains(env.Error.Message, "保存模型定价") {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	for _, secret := range []string{"pq:", "cliproxy", "/etc/secret.db"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("internal error response leaked %q: %s", secret, rec.Body.String())
		}
	}
}

func TestWriteErrorHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("missing field", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeMissingFieldError(c, "api_key", "请提供有效的 API Key")
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if rec.Code != http.StatusBadRequest || env.Error.Code != "missing_required_field" || !strings.Contains(env.Error.Message, "api_key") {
			t.Fatalf("got %d %+v", rec.Code, env)
		}
	})

	t.Run("not found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeNotFoundError(c, "代理条目", "proxy-1")
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if rec.Code != http.StatusNotFound || env.Error.Code != "not_found" || !strings.Contains(env.Error.Message, "proxy-1") {
			t.Fatalf("got %d %+v", rec.Code, env)
		}
	})

	t.Run("service unavailable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeServiceUnavailableError(c, "终端用户服务")
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if rec.Code != http.StatusServiceUnavailable || env.Error.Code != "service_unavailable" || !strings.Contains(env.Error.Message, "终端用户服务") {
			t.Fatalf("got %d %+v", rec.Code, env)
		}
	})

	t.Run("read body too large", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeReadBodyError(c, bodyutil.ErrBodyTooLarge, 8<<10)
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if rec.Code != http.StatusRequestEntityTooLarge || env.Error.Code != "request_body_too_large" || !strings.Contains(env.Error.Message, "8 KiB") {
			t.Fatalf("got %d %+v", rec.Code, env)
		}
	})

	t.Run("upstream error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeUpstreamError(c, http.StatusBadGateway, "查询上游额度", context.DeadlineExceeded)
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if rec.Code != http.StatusBadGateway || env.Error.Code != "upstream_timeout" || !strings.Contains(env.Error.Message, "查询上游额度") {
			t.Fatalf("got %d %+v", rec.Code, env)
		}
	})
}
