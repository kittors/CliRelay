package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestPolicyAction string

const (
	requestPolicyActionSkipChannel requestPolicyAction = "skip-channel"
	requestPolicyActionReject      requestPolicyAction = "reject"
)

type requestPolicyLimitError struct {
	policy           string
	requestedModel   string
	upstreamProvider string
	upstreamModel    string
	requestBytes     int64
	maxRequestBytes  int64
	reason           string
	action           requestPolicyAction
}

func (e *requestPolicyLimitError) Error() string {
	if e == nil {
		return ""
	}
	message := fmt.Sprintf("request policy %s blocked upstream model %s via provider %s", e.policy, e.upstreamModel, e.upstreamProvider)
	if e.reason != "" {
		message = fmt.Sprintf("%s: %s", message, e.reason)
	} else if e.maxRequestBytes > 0 {
		message = fmt.Sprintf("request body is too large for upstream model %s via provider %s: %d bytes exceeds max-request-bytes %d",
			e.upstreamModel, e.upstreamProvider, e.requestBytes, e.maxRequestBytes)
	}
	code := "request_policy_blocked"
	if e.maxRequestBytes > 0 && e.requestBytes > e.maxRequestBytes {
		code = "request_too_large"
	}
	payload := map[string]any{
		"error": map[string]any{
			"message":           message,
			"type":              "invalid_request_error",
			"code":              code,
			"policy":            e.policy,
			"requested_model":   e.requestedModel,
			"upstream_provider": e.upstreamProvider,
			"upstream_model":    e.upstreamModel,
			"request_bytes":     e.requestBytes,
			"max_request_bytes": e.maxRequestBytes,
			"reason":            e.reason,
			"over_limit_action": string(e.action),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return message
	}
	return string(data)
}

func (e *requestPolicyLimitError) StatusCode() int {
	return http.StatusRequestEntityTooLarge
}

func (e *requestPolicyLimitError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return headers
}

func requestPolicyDecision(cfg *internalconfig.Config, auth *Auth, opts cliproxyexecutor.Options, requestedModel, upstreamProvider, upstreamModel string) (bool, *requestPolicyLimitError) {
	if cfg == nil || len(cfg.RequestPolicies) == 0 || auth == nil {
		return false, nil
	}
	requestBytes, _ := requestBytesFromMetadata(opts.Metadata)
	requestedModel = strings.TrimSpace(requestedModel)
	upstreamProvider = strings.ToLower(strings.TrimSpace(upstreamProvider))
	upstreamModel = strings.TrimSpace(upstreamModel)
	for i := range cfg.RequestPolicies {
		policy := cfg.RequestPolicies[i]
		if !requestPolicyMatches(policy, requestedModel, upstreamProvider, upstreamModel) {
			continue
		}
		triggered, reason := requestPolicyTriggered(policy, opts.Metadata, requestBytes)
		if !triggered {
			continue
		}
		action := requestPolicyAction(strings.ToLower(strings.TrimSpace(policy.OverLimit.Action)))
		if action == "" {
			action = requestPolicyActionSkipChannel
		}
		if action != requestPolicyActionReject {
			action = requestPolicyActionSkipChannel
		}
		return true, &requestPolicyLimitError{
			policy:           strings.TrimSpace(policy.Name),
			requestedModel:   requestedModel,
			upstreamProvider: upstreamProvider,
			upstreamModel:    upstreamModel,
			requestBytes:     requestBytes,
			maxRequestBytes:  policy.Limits.MaxRequestBytes,
			reason:           reason,
			action:           action,
		}
	}
	return false, nil
}

func requestPolicyMatches(policy internalconfig.RequestPolicy, requestedModel, upstreamProvider, upstreamModel string) bool {
	if !policyValuesMatchModel(policy.Match.RequestedModels, requestedModel) {
		return false
	}
	if !policyValuesMatchString(policy.Match.UpstreamProviders, upstreamProvider) {
		return false
	}
	if !policyValuesMatchModel(policy.Match.UpstreamModels, upstreamModel) {
		return false
	}
	return true
}

func requestPolicyTriggered(policy internalconfig.RequestPolicy, meta map[string]any, requestBytes int64) (bool, string) {
	features := requestFeaturesFromMetadata(meta)
	if !requestFeaturesMatch(policy.Match.RequestFeatures, features) {
		return false, ""
	}
	if policy.Limits.MaxRequestBytes > 0 && requestBytes > policy.Limits.MaxRequestBytes {
		return true, fmt.Sprintf("request_bytes %d exceeds max-request-bytes %d", requestBytes, policy.Limits.MaxRequestBytes)
	}
	if policy.Limits.MinRequestBytes > 0 && requestBytes >= policy.Limits.MinRequestBytes {
		return true, fmt.Sprintf("request_bytes %d reached min-request-bytes %d", requestBytes, policy.Limits.MinRequestBytes)
	}
	if policy.Limits.MinInputItems > 0 {
		if inputItems, ok := intFromMetadata(meta, cliproxyexecutor.InputItemsMetadataKey); ok && inputItems >= policy.Limits.MinInputItems {
			return true, fmt.Sprintf("input_items %d reached min-input-items %d", inputItems, policy.Limits.MinInputItems)
		}
	}
	if policy.Limits.MinToolCalls > 0 {
		if toolCalls, ok := intFromMetadata(meta, cliproxyexecutor.ToolCallsMetadataKey); ok && toolCalls >= policy.Limits.MinToolCalls {
			return true, fmt.Sprintf("tool_calls %d reached min-tool-calls %d", toolCalls, policy.Limits.MinToolCalls)
		}
	}
	if len(policy.Match.RequestFeatures) > 0 {
		return true, "request feature matched"
	}
	return false, ""
}

func requestFeaturesMatch(required []string, actual []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(actual))
	for _, feature := range actual {
		feature = strings.ToLower(strings.TrimSpace(feature))
		if feature != "" {
			have[feature] = struct{}{}
		}
	}
	for _, feature := range required {
		feature = strings.ToLower(strings.TrimSpace(feature))
		if feature == "" {
			continue
		}
		if _, ok := have[feature]; !ok {
			return false
		}
	}
	return true
}

func policyValuesMatchString(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return true
		}
	}
	return false
}

func policyValuesMatchModel(values []string, model string) bool {
	if len(values) == 0 {
		return true
	}
	model = canonicalPolicyModel(model)
	for _, candidate := range values {
		if canonicalPolicyModel(candidate) == model {
			return true
		}
	}
	return false
}

func canonicalPolicyModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	if strings.TrimSpace(parsed.ModelName) != "" {
		model = parsed.ModelName
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func requestBytesFromMetadata(meta map[string]any) (int64, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	raw, ok := meta[cliproxyexecutor.RequestBytesMetadataKey]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case float64:
		return int64(v), true
	case json.Number:
		parsed, err := v.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func requestFeaturesFromMetadata(meta map[string]any) []string {
	if len(meta) == 0 {
		return nil
	}
	raw := meta[cliproxyexecutor.RequestFeaturesMetadataKey]
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
	default:
		return nil
	}
}

func intFromMetadata(meta map[string]any, key string) (int, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	raw := meta[key]
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case int32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		parsed, err := v.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		return parsed, err == nil
	default:
		return 0, false
	}
}
