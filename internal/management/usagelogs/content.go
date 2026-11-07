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
		return LogContentResponse{Status: http.StatusBadRequest, Payload: map[string]any{"error": "format=text requires part=input, part=output, or part=details"}}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForTenant(s.tenantID, id)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
		}
		result.InputContent = usage.MaskSharedAccountText(s.tenantID, result.InputContent)
		result.OutputContent = usage.MaskSharedAccountText(s.tenantID, result.OutputContent)
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForTenant(s.tenantID, id, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
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
		return LogContentResponse{Status: http.StatusForbidden, Payload: map[string]any{"error": "request details are only available in the management API"}}
	}
	if format == "text" && part == "both" {
		return LogContentResponse{Status: http.StatusBadRequest, Payload: map[string]any{"error": "format=text requires part=input or part=output"}}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForKey(id, apiKey)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
		}
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForKey(id, apiKey, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
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
		return LogContentResponse{Status: http.StatusForbidden, Payload: map[string]any{"error": "request details are only available in the management API"}}
	}
	if format == "text" && part == "both" {
		return LogContentResponse{Status: http.StatusBadRequest, Payload: map[string]any{"error": "format=text requires part=input or part=output"}}
	}
	if part == "both" {
		result, err := usage.QueryLogContentForEndUser(s.tenantID, endUserID, id)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
			}
			return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
		}
		return LogContentResponse{Status: http.StatusOK, Payload: result}
	}

	result, err := usage.QueryLogContentPartForEndUser(s.tenantID, endUserID, id, part)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return LogContentResponse{Status: http.StatusNotFound, Payload: map[string]any{"error": "log entry not found"}}
		}
		return LogContentResponse{Status: http.StatusInternalServerError, Payload: map[string]any{"error": err.Error()}}
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
