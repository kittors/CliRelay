package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	log "github.com/sirupsen/logrus"
)

type apiErrorDetail = managementapitools.APIErrorDetail
type apiErrorEnvelope = managementapitools.APIErrorPayload

// writeError sends the unified structured error payload:
// {"error": {"code": code, "message": message, "details": ...}}.
// Compatible with the admin panel's extractApiErrorCode, extractApiErrorMessage,
// and extractApiErrorDetails helpers.
func writeError(c *gin.Context, status int, code, message string, details ...map[string]any) {
	c.JSON(status, managementapitools.NewAPIError(code, message, details...))
}

// abortError aborts middleware execution with the unified structured error payload.
func abortError(c *gin.Context, status int, code, message string, details ...map[string]any) {
	c.AbortWithStatusJSON(status, managementapitools.NewAPIError(code, message, details...))
}

// describeInvalidJSONBody turns a JSON decode error into a human-readable message.
func describeInvalidJSONBody(err error) string {
	if err == nil {
		return "请求体格式无效，请传入合法的 JSON 对象"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "请求体为空或不完整，请传入有效的 JSON 数据"
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("请求体 JSON 语法有误（第 %d 字节附近），请检查引号、逗号或括号是否匹配", syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := strings.TrimSpace(typeErr.Field)
		expected := friendlyGoTypeName(typeErr.Type)
		if field != "" {
			return fmt.Sprintf("请求参数 %q 的类型不正确，期望为%s类型，实际收到 %q", field, expected, typeErr.Value)
		}
		return fmt.Sprintf("请求体根类型不正确，期望为%s类型，实际收到 %q", expected, typeErr.Value)
	}
	return "请求体格式无效，请检查 JSON 结构和必填字段类型是否正确"
}

// writeInvalidBodyError explains why JSON request body decoding failed instead
// of returning a bare "invalid body" string.
func writeInvalidBodyError(c *gin.Context, err error) {
	if err == nil {
		writeError(c, http.StatusBadRequest, "invalid_request_body", describeInvalidJSONBody(nil))
		return
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		writeError(c, http.StatusBadRequest, "empty_request_body", describeInvalidJSONBody(err))
		return
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		writeError(c, http.StatusBadRequest, "invalid_json_syntax",
			describeInvalidJSONBody(err),
			map[string]any{"offset": syntaxErr.Offset},
		)
		return
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := strings.TrimSpace(typeErr.Field)
		expected := friendlyGoTypeName(typeErr.Type)
		if field != "" {
			writeError(c, http.StatusBadRequest, "invalid_field_type",
				describeInvalidJSONBody(err),
				map[string]any{"field": field, "expected_type": expected, "actual_value": typeErr.Value},
			)
			return
		}
		writeError(c, http.StatusBadRequest, "invalid_field_type", describeInvalidJSONBody(err))
		return
	}
	writeError(c, http.StatusBadRequest, "invalid_request_body", describeInvalidJSONBody(err))
}

// writeReadBodyError reports body-read or body-size-limit failures clearly.
func writeReadBodyError(c *gin.Context, err error, limitBytes int64) {
	if bodyutil.IsTooLarge(err) {
		if limitBytes > 0 {
			writeError(c, http.StatusRequestEntityTooLarge, "request_body_too_large",
				fmt.Sprintf("请求体大小超出限制（最大允许 %s），请精简提交内容后重试", formatByteLimit(limitBytes)),
				map[string]any{"max_bytes": limitBytes},
			)
			return
		}
		writeError(c, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体大小超出服务端限制，请精简提交内容后重试")
		return
	}
	writeError(c, http.StatusBadRequest, "read_body_failed", "读取请求体失败，可能是客户端连接被提前中断，请重试")
}

// writeMissingFieldError tells callers exactly which required parameter is missing.
func writeMissingFieldError(c *gin.Context, field, hint string) {
	field = strings.TrimSpace(field)
	hint = strings.TrimSpace(hint)
	var msg string
	switch {
	case field != "" && hint != "":
		msg = fmt.Sprintf("缺少必填参数 %q：%s", field, hint)
	case field != "":
		msg = fmt.Sprintf("缺少必填参数 %q，请补充后重试", field)
	case hint != "":
		msg = "缺少必填参数：" + hint
	default:
		msg = "缺少必填参数，请检查请求内容后重试"
	}
	var details map[string]any
	if field != "" {
		details = map[string]any{"field": field}
	}
	writeError(c, http.StatusBadRequest, "missing_required_field", msg, details)
}

// writeInvalidFieldError tells callers which parameter failed validation and why.
func writeInvalidFieldError(c *gin.Context, field, reason string) {
	field = strings.TrimSpace(field)
	reason = strings.TrimSpace(reason)
	var msg string
	switch {
	case field != "" && reason != "":
		msg = fmt.Sprintf("参数 %q 无效：%s", field, reason)
	case field != "":
		msg = fmt.Sprintf("参数 %q 格式或取值无效，请检查后重试", field)
	case reason != "":
		msg = "请求参数无效：" + reason
	default:
		msg = "请求参数无效，请检查后重试"
	}
	var details map[string]any
	if field != "" {
		details = map[string]any{"field": field}
	}
	writeError(c, http.StatusBadRequest, "invalid_field", msg, details)
}

// writeNotFoundError tells callers which resource could not be found in the current scope.
func writeNotFoundError(c *gin.Context, resourceLabel, identifier string) {
	resourceLabel = strings.TrimSpace(resourceLabel)
	identifier = strings.TrimSpace(identifier)
	if resourceLabel == "" {
		resourceLabel = "资源"
	}
	var msg string
	var details map[string]any
	if identifier != "" {
		msg = fmt.Sprintf("未找到指定的%s（%s），可能已被删除或不属于当前租户工作区", resourceLabel, identifier)
		details = map[string]any{"resource": resourceLabel, "identifier": identifier}
	} else {
		msg = fmt.Sprintf("未找到目标%s，可能已被删除或不属于当前租户工作区", resourceLabel)
		details = map[string]any{"resource": resourceLabel}
	}
	writeError(c, http.StatusNotFound, "not_found", msg, details)
}

// writeServiceUnavailableError reports uninitialized or unavailable backend components.
func writeServiceUnavailableError(c *gin.Context, serviceLabel string) {
	serviceLabel = strings.TrimSpace(serviceLabel)
	if serviceLabel == "" {
		serviceLabel = "相关服务"
	}
	writeError(c, http.StatusServiceUnavailable, "service_unavailable",
		fmt.Sprintf("%s尚未就绪或未配置，请检查服务端配置和运行状态后重试", serviceLabel),
	)
}

// writeInternalError logs the full underlying error server-side and returns a
// clear, sanitized error message to the client without leaking raw SQL, file
// paths, or internal stack traces.
func writeInternalError(c *gin.Context, actionLabel string, err error) {
	writeInternalErrorWithCode(c, "internal_error", actionLabel, err)
}

// writeInternalErrorWithCode behaves like writeInternalError while preserving a
// caller-specific machine error code for frontend translation hooks.
func writeInternalErrorWithCode(c *gin.Context, code, actionLabel string, err error) {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "internal_error"
	}
	actionLabel = strings.TrimSpace(actionLabel)
	if actionLabel == "" {
		actionLabel = "处理请求"
	}
	fields := log.Fields{"action": actionLabel, "code": code}
	if c != nil && c.Request != nil {
		fields["method"] = c.Request.Method
		fields["path"] = c.Request.URL.Path
	}
	if err != nil {
		log.WithFields(fields).WithError(err).Error("management handler internal error")
	} else {
		log.WithFields(fields).Error("management handler internal error")
	}
	writeError(c, http.StatusInternalServerError, code,
		fmt.Sprintf("%s失败，服务端内部处理异常，请稍后重试或查看服务端日志排查", actionLabel),
	)
}

// writeUpstreamError classifies an upstream network failure, logs details
// server-side, and returns an actionable error to the client.
func writeUpstreamError(c *gin.Context, status int, actionLabel string, err error) {
	code, reason := describeUpstreamNetworkError(err)
	actionLabel = strings.TrimSpace(actionLabel)
	fields := log.Fields{"action": actionLabel, "code": code}
	if c != nil && c.Request != nil {
		fields["method"] = c.Request.Method
		fields["path"] = c.Request.URL.Path
	}
	if err != nil {
		log.WithFields(fields).WithError(err).Warn("management upstream request failed")
	}
	msg := reason
	if actionLabel != "" {
		msg = fmt.Sprintf("%s失败：%s", actionLabel, reason)
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	writeError(c, status, code, msg)
}

func describeUpstreamNetworkError(err error) (string, string) {
	return managementapitools.DescribeUpstreamNetworkError(err)
}

func friendlyGoTypeName(t reflect.Type) string {
	if t == nil {
		return "有效数据"
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "字符串"
	case reflect.Bool:
		return "布尔值"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "整数"
	case reflect.Float32, reflect.Float64:
		return "数字"
	case reflect.Slice, reflect.Array:
		return "数组"
	case reflect.Map, reflect.Struct:
		return "对象"
	default:
		return t.String()
	}
}

func formatByteLimit(limitBytes int64) string {
	const (
		kiB = 1024
		miB = 1024 * kiB
	)
	switch {
	case limitBytes >= miB && limitBytes%miB == 0:
		return fmt.Sprintf("%d MiB", limitBytes/miB)
	case limitBytes >= miB:
		return fmt.Sprintf("%.1f MiB", float64(limitBytes)/float64(miB))
	case limitBytes >= kiB && limitBytes%kiB == 0:
		return fmt.Sprintf("%d KiB", limitBytes/kiB)
	case limitBytes >= kiB:
		return fmt.Sprintf("%.1f KiB", float64(limitBytes)/float64(kiB))
	default:
		return fmt.Sprintf("%d 字节", limitBytes)
	}
}
