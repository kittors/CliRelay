package usagelogs

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func NormalizeLogContentFormatValue(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return "json"
	}
	switch format {
	case "json", "text":
		return format
	default:
		return "json"
	}
}

func NormalizeLogContentPartValue(part string) string {
	part = strings.ToLower(strings.TrimSpace(part))
	if part == "" {
		return "both"
	}
	switch part {
	case "both", "input", "output", "details":
		return part
	default:
		return "both"
	}
}

func (s *Service) LogContent(id int64, part, format string) LogContentResponse {
	if format == "text" && part == "both" {
		return LogContentResponse{Status: http.StatusBadRequest, Payload: errorPayload("invalid_log_content_format", "以纯文本（format=text）读取日志内容时，必须明确指定 part=input、part=output 或 part=details")}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForTenant(s.tenantID, id)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到对应的请求日志内容，可能尚未开启内容存储、已被清理或不属于当前租户")}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询请求日志内容", err)}
		}
		result.InputContent = usage.MaskSharedAccountText(s.tenantID, result.InputContent)
		result.OutputContent = usage.MaskSharedAccountText(s.tenantID, result.OutputContent)
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForTenant(s.tenantID, id, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到对应的请求日志片段，可能尚未开启内容存储、已被清理或不属于当前租户")}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询请求日志片段", err)}
	}

	result.Content = usage.MaskSharedAccountText(s.tenantID, result.Content)
	if format == "text" {
		headers := map[string]string{
			"X-Log-Id":   strconv.FormatInt(result.ID, 10),
			"X-Log-Part": result.Part,
		}
		if strings.TrimSpace(result.Model) != "" {
			headers["X-Model"] = result.Model
		}
		return LogContentResponse{
			Status:      http.StatusOK,
			ContentType: "text/plain; charset=utf-8",
			Headers:     headers,
			Text:        result.Content,
		}
	}

	return LogContentResponse{Status: http.StatusOK, Payload: result}
}

func (s *Service) PublicLogContent(id int64, apiKey, part, format string) LogContentResponse {
	if part == "details" {
		return LogContentResponse{Status: http.StatusForbidden, Payload: errorPayload("log_details_forbidden", "请求诊断详情（part=details）仅限管理后台访问，公开查询接口不支持查看")}
	}
	if format == "text" && part == "both" {
		return LogContentResponse{Status: http.StatusBadRequest, Payload: errorPayload("invalid_log_content_format", "以纯文本（format=text）读取日志内容时，必须明确指定 part=input 或 part=output")}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForKey(id, apiKey)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到该 API Key 对应的请求日志内容，可能已被清理或无权访问")}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询公开请求日志内容", err)}
		}
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForKey(id, apiKey, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到该 API Key 对应的请求日志片段，可能已被清理或无权访问")}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询公开请求日志片段", err)}
	}

	if format == "text" {
		headers := map[string]string{
			"X-Log-Id":   strconv.FormatInt(result.ID, 10),
			"X-Log-Part": result.Part,
		}
		if strings.TrimSpace(result.Model) != "" {
			headers["X-Model"] = result.Model
		}
		return LogContentResponse{
			Status:      http.StatusOK,
			ContentType: "text/plain; charset=utf-8",
			Headers:     headers,
			Text:        result.Content,
		}
	}

	return LogContentResponse{Status: http.StatusOK, Payload: result}
}

func (s *Service) PublicLogContentForEndUser(id int64, endUserID, part, format string) LogContentResponse {
	if part == "details" {
		return LogContentResponse{Status: http.StatusForbidden, Payload: errorPayload("log_details_forbidden", "请求诊断详情（part=details）仅限管理后台访问，用户门户不支持查看")}
	}
	if format == "text" && part == "both" {
		return LogContentResponse{Status: http.StatusBadRequest, Payload: errorPayload("invalid_log_content_format", "以纯文本（format=text）读取日志内容时，必须明确指定 part=input 或 part=output")}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForEndUser(s.tenantID, endUserID, id)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到该账号对应的请求日志内容，可能已被清理或无权访问")}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询门户请求日志内容", err)}
		}
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForEndUser(s.tenantID, endUserID, id, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: errorPayload("log_entry_not_found", "未找到该账号对应的请求日志片段，可能已被清理或无权访问")}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: internalErrorPayload("查询门户请求日志片段", err)}
	}
	if format == "text" {
		headers := map[string]string{
			"X-Log-Id":   strconv.FormatInt(result.ID, 10),
			"X-Log-Part": result.Part,
		}
		if strings.TrimSpace(result.Model) != "" {
			headers["X-Model"] = result.Model
		}
		return LogContentResponse{
			Status:      http.StatusOK,
			ContentType: "text/plain; charset=utf-8",
			Headers:     headers,
			Text:        result.Content,
		}
	}
	return LogContentResponse{Status: http.StatusOK, Payload: result}
}
